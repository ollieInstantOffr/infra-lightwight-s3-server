package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Versioning keeps the history of a key rather than overwriting it.
//
// This is version history, not redundancy: each distinct version still has
// exactly one copy of its bytes, so it does not soften the single-copy
// guarantee. What it does change is that deleting an object no longer frees its
// space — the bytes stay referenced by the old version until it is purged. The
// console says so where versioning is enabled, because a storage screen that
// quietly stops reclaiming space is a bad surprise.

// ErrVersionNotFound means no such version exists for that key.
var ErrVersionNotFound = errors.New("object version not found")

// ObjectVersion is one point in a key's history.
type ObjectVersion struct {
	ID             string
	BucketID       string
	Key            string
	VersionID      string
	BlobDigest     *string
	Size           int64
	ETag           string
	ContentType    string
	Metadata       map[string]string
	IsDeleteMarker bool
	CreatedAt      time.Time
	CreatedBy      string
}

// NewVersionID returns an opaque version identifier.
func NewVersionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate version id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// RecordVersion appends to a key's history and retains the blob it points at.
//
// Called inside the caller's transaction, alongside the object write, so a
// version and the object it describes appear together or not at all.
func RecordVersion(ctx context.Context, q Querier, version *ObjectVersion) error {
	metadata, err := json.Marshal(version.Metadata)
	if err != nil {
		return fmt.Errorf("encode version metadata: %w", err)
	}
	if version.VersionID == "" {
		if version.VersionID, err = NewVersionID(); err != nil {
			return err
		}
	}

	// A version holds its own reference to the blob, independent of the live
	// object's. That is what keeps the bytes alive after the object moves on.
	if version.BlobDigest != nil {
		if err := RetainBlob(ctx, q, *version.BlobDigest, version.Size); err != nil {
			return err
		}
	}

	err = q.QueryRow(ctx, `
		INSERT INTO object_versions
			(bucket_id, key, version_id, blob_digest, size, etag, content_type,
			 metadata, is_delete_marker, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id::text, created_at`,
		version.BucketID, version.Key, version.VersionID, version.BlobDigest,
		version.Size, version.ETag, version.ContentType, metadata,
		version.IsDeleteMarker, version.CreatedBy,
	).Scan(&version.ID, &version.CreatedAt)
	if err != nil {
		return fmt.Errorf("record object version: %w", err)
	}
	return nil
}

// ListVersions returns a key's history, newest first.
func ListVersions(ctx context.Context, q Querier, bucketID, key string, limit int) ([]ObjectVersion, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `
		SELECT id::text, key, version_id, blob_digest, size, etag, content_type,
		       metadata, is_delete_marker, created_at, created_by
		FROM object_versions
		WHERE bucket_id = $1 AND key = $2
		ORDER BY created_at DESC
		LIMIT $3`, bucketID, key, limit)
	if err != nil {
		return nil, fmt.Errorf("list object versions: %w", err)
	}
	defer rows.Close()

	return scanVersions(rows, bucketID)
}

// ListBucketVersions returns recent versions across a bucket, which is what the
// "show versions" listing renders.
func ListBucketVersions(ctx context.Context, q Querier, bucketID, prefix string, limit int) ([]ObjectVersion, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := q.Query(ctx, `
		SELECT id::text, key, version_id, blob_digest, size, etag, content_type,
		       metadata, is_delete_marker, created_at, created_by
		FROM object_versions
		WHERE bucket_id = $1 AND ($2 = '' OR (key >= $2 AND key < $3))
		ORDER BY key, created_at DESC
		LIMIT $4`, bucketID, prefix, prefixUpperBound(prefix), limit)
	if err != nil {
		return nil, fmt.Errorf("list bucket versions: %w", err)
	}
	defer rows.Close()

	return scanVersions(rows, bucketID)
}

// GetVersion fetches one specific version.
func GetVersion(ctx context.Context, q Querier, bucketID, key, versionID string) (*ObjectVersion, error) {
	version := &ObjectVersion{BucketID: bucketID}
	var metadata []byte

	err := q.QueryRow(ctx, `
		SELECT id::text, key, version_id, blob_digest, size, etag, content_type,
		       metadata, is_delete_marker, created_at, created_by
		FROM object_versions
		WHERE bucket_id = $1 AND key = $2 AND version_id = $3`,
		bucketID, key, versionID,
	).Scan(&version.ID, &version.Key, &version.VersionID, &version.BlobDigest, &version.Size,
		&version.ETag, &version.ContentType, &metadata, &version.IsDeleteMarker,
		&version.CreatedAt, &version.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrVersionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get object version: %w", err)
	}
	if len(metadata) > 0 {
		_ = json.Unmarshal(metadata, &version.Metadata)
	}
	return version, nil
}

// RestoreVersion makes an old version current again.
//
// It copies rather than moves: the history stays intact, and the restore
// itself becomes the newest version. Undoing a restore is then just another
// restore, rather than an operation with no inverse.
func RestoreVersion(ctx context.Context, pool *Pool, bucketID, key, versionID, actor string) (*Object, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin restore: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, objectLockID(bucketID, key)); err != nil {
		return nil, fmt.Errorf("lock object key: %w", err)
	}

	version, err := GetVersion(ctx, tx, bucketID, key, versionID)
	if err != nil {
		return nil, err
	}
	if version.IsDeleteMarker || version.BlobDigest == nil {
		return nil, fmt.Errorf("%w: that version is a delete marker and has no contents", ErrVersionNotFound)
	}

	var previousDigest string
	err = tx.QueryRow(ctx, `SELECT blob_digest FROM objects WHERE bucket_id = $1 AND key = $2`,
		bucketID, key).Scan(&previousDigest)
	hadPrevious := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read existing object: %w", err)
	}

	if err := RetainBlob(ctx, tx, *version.BlobDigest, version.Size); err != nil {
		return nil, err
	}

	metadata, err := json.Marshal(version.Metadata)
	if err != nil {
		return nil, fmt.Errorf("encode metadata: %w", err)
	}

	object := &Object{
		BucketID: bucketID, Key: key, BlobDigest: *version.BlobDigest,
		Size: version.Size, ETag: version.ETag, ContentType: version.ContentType,
		Metadata: version.Metadata,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO objects (bucket_id, key, blob_digest, size, etag, content_type, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (bucket_id, key) DO UPDATE SET
			blob_digest = EXCLUDED.blob_digest, size = EXCLUDED.size,
			etag = EXCLUDED.etag, content_type = EXCLUDED.content_type,
			metadata = EXCLUDED.metadata, updated_at = now()
		RETURNING id::text, created_at, updated_at`,
		bucketID, key, object.BlobDigest, object.Size, object.ETag, object.ContentType, metadata,
	).Scan(&object.ID, &object.CreatedAt, &object.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("restore object: %w", err)
	}

	if hadPrevious {
		if _, err := ReleaseBlob(ctx, tx, previousDigest); err != nil {
			return nil, err
		}
	}

	// The restore is itself a new version, so the history reads as a sequence
	// of states rather than jumping backwards.
	restored := &ObjectVersion{
		BucketID: bucketID, Key: key, BlobDigest: version.BlobDigest,
		Size: version.Size, ETag: version.ETag, ContentType: version.ContentType,
		Metadata: version.Metadata, CreatedBy: actor,
	}
	if err := RecordVersion(ctx, tx, restored); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit restore: %w", err)
	}
	return object, nil
}

// PurgeVersion permanently removes one version and releases its blob.
func PurgeVersion(ctx context.Context, pool *Pool, bucketID, key, versionID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin purge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var digest *string
	err = tx.QueryRow(ctx, `
		DELETE FROM object_versions
		WHERE bucket_id = $1 AND key = $2 AND version_id = $3
		RETURNING blob_digest`, bucketID, key, versionID).Scan(&digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrVersionNotFound
	}
	if err != nil {
		return fmt.Errorf("purge version: %w", err)
	}

	if digest != nil {
		if _, err := ReleaseBlob(ctx, tx, *digest); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// PurgeKeyVersions removes every version of a key, releasing all their blobs.
// This is what actually reclaims the space a versioned delete left behind.
func PurgeKeyVersions(ctx context.Context, pool *Pool, bucketID, key string) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin purge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		DELETE FROM object_versions
		WHERE bucket_id = $1 AND key = $2
		RETURNING blob_digest`, bucketID, key)
	if err != nil {
		return 0, fmt.Errorf("purge key versions: %w", err)
	}

	var digests []string
	for rows.Next() {
		var digest *string
		if err := rows.Scan(&digest); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan purged version: %w", err)
		}
		if digest != nil {
			digests = append(digests, *digest)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, digest := range digests {
		if _, err := ReleaseBlob(ctx, tx, digest); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit purge: %w", err)
	}
	return len(digests), nil
}

// VersionedSpace reports how many bytes are held by versions that are no longer
// the live object, which is the number an operator needs before deciding
// whether to purge.
func VersionedSpace(ctx context.Context, q Querier, bucketID string) (bytes int64, versions int64, err error) {
	err = q.QueryRow(ctx, `
		SELECT coalesce(sum(v.size), 0), count(*)
		FROM object_versions v
		LEFT JOIN objects o ON o.bucket_id = v.bucket_id AND o.key = v.key
		WHERE v.bucket_id = $1
		  AND NOT v.is_delete_marker
		  AND (o.blob_digest IS NULL OR o.blob_digest <> v.blob_digest)`,
		bucketID).Scan(&bytes, &versions)
	if err != nil {
		return 0, 0, fmt.Errorf("measure versioned space: %w", err)
	}
	return bytes, versions, nil
}

func scanVersions(rows pgx.Rows, bucketID string) ([]ObjectVersion, error) {
	var out []ObjectVersion
	for rows.Next() {
		version := ObjectVersion{BucketID: bucketID}
		var metadata []byte
		if err := rows.Scan(&version.ID, &version.Key, &version.VersionID, &version.BlobDigest,
			&version.Size, &version.ETag, &version.ContentType, &metadata,
			&version.IsDeleteMarker, &version.CreatedAt, &version.CreatedBy); err != nil {
			return nil, fmt.Errorf("scan object version: %w", err)
		}
		if len(metadata) > 0 {
			_ = json.Unmarshal(metadata, &version.Metadata)
		}
		out = append(out, version)
	}
	return out, rows.Err()
}
