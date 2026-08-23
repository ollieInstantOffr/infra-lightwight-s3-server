package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"time"

	"github.com/jackc/pgx/v5"
)

// Addressing a specific version, rather than whatever is current.
//
// The awkwardness here comes from the current version living in a different
// table from the rest. That split earns its keep — every ordinary GET is a
// single indexed lookup with no version reasoning at all — but it means the
// operations below have to look in two places and keep the invariant that
// exactly one version of a key is current.

// ErrIsDeleteMarker means the requested version is a delete marker. S3 answers
// a GET of one with 405, not 404: the version exists, it just has no body.
var ErrIsDeleteMarker = errors.New("that version is a delete marker")

// GetObjectVersion fetches one version of a key by id, current or not.
//
// The returned Object carries the version's state. IsDeleteMarker is reported
// separately because a delete marker is a real version that a client may
// address and delete, but is not something that can be served.
func GetObjectVersion(ctx context.Context, q Querier, bucketID, key, versionID string) (*Object, error) {
	stored := internalVersionID(versionID)

	// The current version first: it is the common case, and the only place a
	// key's live state lives.
	current, err := readCurrent(ctx, q, bucketID, key)
	if err != nil {
		return nil, err
	}
	if current.Exists && current.VersionID == stored {
		object := current.Object
		object.BucketID, object.Key, object.VersionID = bucketID, key, current.VersionID
		return &object, nil
	}

	version, err := GetVersion(ctx, q, bucketID, key, versionOrNull(stored))
	if err != nil {
		return nil, err
	}
	if version.IsDeleteMarker {
		return nil, ErrIsDeleteMarker
	}
	return &Object{
		BucketID: bucketID, Key: key, BlobDigest: *version.BlobDigest,
		Size: version.Size, ETag: version.ETag, ContentType: version.ContentType,
		Metadata: version.Metadata, VersionID: stored,
		CreatedAt: version.CreatedAt, UpdatedAt: version.CreatedAt,
	}, nil
}

// versionOrNull maps the empty stored id onto the literal used in the versions
// table, where the null version is written as "null" so the unique constraint
// on (bucket, key, version) has something to work with.
func versionOrNull(stored string) string {
	if stored == "" {
		return NullVersionID
	}
	return stored
}

// DeleteObjectVersion permanently removes one version of a key.
//
// This is the only operation that actually destroys data on a versioned bucket,
// and it is also how a delete is undone: removing a delete marker promotes
// whatever was underneath it back to current, and the object reappears.
func DeleteObjectVersion(ctx context.Context, pool *Pool, bucketID, key, versionID string) (Deletion, error) {
	var result Deletion
	stored := internalVersionID(versionID)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("begin delete version: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`,
		objectLockID(bucketID, key)); err != nil {
		return result, fmt.Errorf("lock object key: %w", err)
	}

	current, err := readCurrent(ctx, tx, bucketID, key)
	if err != nil {
		return result, err
	}

	switch {
	case current.Exists && current.VersionID == stored:
		// Removing the current version. Its successor has to be promoted, or
		// the key would appear to have no current version while older ones
		// still exist — which S3 never does.
		if _, err := tx.Exec(ctx, `DELETE FROM objects WHERE bucket_id = $1 AND key = $2`,
			bucketID, key); err != nil {
			return result, fmt.Errorf("delete current version: %w", err)
		}
		if _, err := ReleaseBlob(ctx, tx, current.BlobDigest); err != nil {
			return result, err
		}
		result.Deleted = true

	default:
		var (
			digest   *string
			isMarker bool
		)
		err := tx.QueryRow(ctx, `
			DELETE FROM object_versions
			WHERE bucket_id = $1 AND key = $2 AND version_id = $3
			RETURNING blob_digest, is_delete_marker`,
			bucketID, key, versionOrNull(stored)).Scan(&digest, &isMarker)
		if errors.Is(err, pgx.ErrNoRows) {
			return result, ErrVersionNotFound
		}
		if err != nil {
			return result, fmt.Errorf("delete version: %w", err)
		}
		if digest != nil {
			if _, err := ReleaseBlob(ctx, tx, *digest); err != nil {
				return result, err
			}
		}
		result.Deleted = true
		result.DeleteMarker = isMarker
	}

	// Promote only when nothing is current. Deleting a non-current version
	// leaves the current one alone.
	if !current.Exists || current.VersionID == stored {
		if err := promoteNewestVersion(ctx, tx, bucketID, key); err != nil {
			return result, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit delete version: %w", err)
	}
	return result, nil
}

// promoteNewestVersion makes the newest remaining version current.
//
// Called when a key has no current version, which happens after removing the
// current one or after removing a delete marker. If the newest remaining
// version is itself a delete marker the key stays deleted, since that marker is
// now the current version and a marker is not something that can be served.
//
// The promoted row moves out of object_versions and into objects, keeping the
// invariant that a version is in exactly one of them — and keeping its id,
// since a promoted version is the same version it always was.
func promoteNewestVersion(ctx context.Context, q Querier, bucketID, key string) error {
	newest := &ObjectVersion{BucketID: bucketID, Key: key}
	var metadata []byte

	err := q.QueryRow(ctx, `
		SELECT id::text, version_id, blob_digest, size, etag, content_type,
		       metadata, is_delete_marker
		FROM object_versions
		WHERE bucket_id = $1 AND key = $2
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, bucketID, key,
	).Scan(&newest.ID, &newest.VersionID, &newest.BlobDigest, &newest.Size,
		&newest.ETag, &newest.ContentType, &metadata, &newest.IsDeleteMarker)
	if errors.Is(err, pgx.ErrNoRows) {
		// No history at all: the key is simply gone.
		return nil
	}
	if err != nil {
		return fmt.Errorf("find newest version: %w", err)
	}
	if newest.IsDeleteMarker {
		// The marker underneath is now current, so the key stays deleted.
		return nil
	}
	if len(metadata) == 0 {
		metadata = []byte("{}")
	}

	// The version already holds a reference; the objects row needs its own, and
	// the version row is about to give its one up.
	if err := RetainBlob(ctx, q, *newest.BlobDigest, newest.Size); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO objects (bucket_id, key, blob_digest, size, etag, content_type, metadata, version_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		bucketID, key, *newest.BlobDigest, newest.Size, newest.ETag,
		newest.ContentType, metadata, internalVersionID(newest.VersionID)); err != nil {
		return fmt.Errorf("promote version: %w", err)
	}
	if _, err := q.Exec(ctx, `DELETE FROM object_versions WHERE id = $1::uuid`, newest.ID); err != nil {
		return fmt.Errorf("clear promoted version: %w", err)
	}
	if _, err := ReleaseBlob(ctx, q, *newest.BlobDigest); err != nil {
		return err
	}
	return nil
}

// VersionEntry is one row of a ListObjectVersions response: a stored version or
// a delete marker, with whether it is the current one.
type VersionEntry struct {
	Key            string
	VersionID      string
	IsLatest       bool
	IsDeleteMarker bool
	Size           int64
	ETag           string
	LastModified   pgtypeTime
	Metadata       map[string]string
	ContentType    string
}

// pgtypeTime keeps the import surface small; it is just time.Time.
type pgtypeTime = time.Time

// decodeVersionMetadata is shared by the listing scans.
func decodeVersionMetadata(raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]string
	_ = json.Unmarshal(raw, &out)
	return out
}
