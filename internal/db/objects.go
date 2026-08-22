package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrObjectNotFound means no object exists at that key.
var ErrObjectNotFound = errors.New("object not found")

// Object is an object's metadata. The bytes live in the blob store, addressed
// by BlobDigest.
type Object struct {
	ID          string
	BucketID    string
	Key         string
	BlobDigest  string
	Size        int64
	ETag        string
	ContentType string
	Metadata    map[string]string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// objectLockID derives an advisory-lock key for one object key within a bucket.
//
// Concurrent writes to the same key must be serialised, and not merely for
// last-write-wins ordering. Without it, two simultaneous first-time PUTs both
// observe no existing row, so neither releases the other's blob reference — the
// loser's blob keeps a reference nothing points at and is never collected. That
// is a slow disk leak rather than a visible failure, which makes it the kind of
// bug that is found months later.
func objectLockID(bucketID, key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(bucketID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	return int64(h.Sum64())
}

// WriteOptions carries the per-bucket behaviour a write depends on.
type WriteOptions struct {
	// Versioning keeps the previous state as history rather than discarding
	// it. The old blob then stays referenced by its version, so the space is
	// not reclaimed until the versions are purged.
	Versioning bool
	// Actor is recorded on the version, so history says who changed what.
	Actor string
}

// PutObject writes or replaces an object.
//
// The blob must already be on disk. Reference counting is adjusted in the same
// transaction as the metadata: the new blob is retained, and any blob the key
// previously pointed at is released — unless versioning is on, in which case
// the previous state becomes a version that holds its own reference.
func PutObject(ctx context.Context, pool *Pool, obj *Object, opts WriteOptions) error {
	metadata, err := json.Marshal(obj.Metadata)
	if err != nil {
		return fmt.Errorf("encode object metadata: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin put object: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`,
		objectLockID(obj.BucketID, obj.Key)); err != nil {
		return fmt.Errorf("lock object key: %w", err)
	}

	// Under the lock, whatever the key points at now is stable.
	var previous Object
	err = tx.QueryRow(ctx, `
		SELECT blob_digest, size, etag, content_type, metadata
		FROM objects WHERE bucket_id = $1 AND key = $2`,
		obj.BucketID, obj.Key).Scan(&previous.BlobDigest, &previous.Size, &previous.ETag,
		&previous.ContentType, &previousMetadataHolder{&previous})
	hadPrevious := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read existing object: %w", err)
	}

	if err := RetainBlob(ctx, tx, obj.BlobDigest, obj.Size); err != nil {
		return err
	}

	// With versioning on, the state being replaced is preserved as a version,
	// which takes its own reference to the old blob before the live one is
	// released below.
	if opts.Versioning && hadPrevious {
		superseded := &ObjectVersion{
			BucketID: obj.BucketID, Key: obj.Key, BlobDigest: &previous.BlobDigest,
			Size: previous.Size, ETag: previous.ETag, ContentType: previous.ContentType,
			Metadata: previous.Metadata, CreatedBy: opts.Actor,
		}
		if err := RecordVersion(ctx, tx, superseded); err != nil {
			return err
		}
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO objects (bucket_id, key, blob_digest, size, etag, content_type, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (bucket_id, key) DO UPDATE SET
			blob_digest  = EXCLUDED.blob_digest,
			size         = EXCLUDED.size,
			etag         = EXCLUDED.etag,
			content_type = EXCLUDED.content_type,
			metadata     = EXCLUDED.metadata,
			updated_at   = now()
		RETURNING id::text, created_at, updated_at`,
		obj.BucketID, obj.Key, obj.BlobDigest, obj.Size, obj.ETag, obj.ContentType, metadata,
	).Scan(&obj.ID, &obj.CreatedAt, &obj.UpdatedAt)
	if err != nil {
		return fmt.Errorf("write object %q: %w", obj.Key, err)
	}

	// Released unconditionally when a row existed, including when the digest is
	// unchanged: an overwrite with identical bytes retained a second reference
	// above, and the key still accounts for exactly one.
	if hadPrevious {
		if _, err := ReleaseBlob(ctx, tx, previous.BlobDigest); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit put object: %w", err)
	}
	return nil
}

// GetObject reads an object's metadata.
func GetObject(ctx context.Context, q Querier, bucketID, key string) (*Object, error) {
	obj := &Object{BucketID: bucketID, Key: key}
	var metadata []byte

	err := q.QueryRow(ctx, `
		SELECT id::text, blob_digest, size, etag, content_type, metadata, created_at, updated_at
		FROM objects WHERE bucket_id = $1 AND key = $2`,
		bucketID, key,
	).Scan(&obj.ID, &obj.BlobDigest, &obj.Size, &obj.ETag, &obj.ContentType,
		&metadata, &obj.CreatedAt, &obj.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}

	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &obj.Metadata); err != nil {
			return nil, fmt.Errorf("decode metadata for %q: %w", key, err)
		}
	}
	return obj, nil
}

// DeleteObject removes an object and releases its blob reference.
//
// Deleting a key that does not exist is not an error: S3 makes DELETE
// idempotent, and clients rely on that when cleaning up.
func DeleteObject(ctx context.Context, pool *Pool, bucketID, key string, opts WriteOptions) (deleted bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin delete object: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`,
		objectLockID(bucketID, key)); err != nil {
		return false, fmt.Errorf("lock object key: %w", err)
	}

	var removed Object
	err = tx.QueryRow(ctx, `
		DELETE FROM objects WHERE bucket_id = $1 AND key = $2
		RETURNING blob_digest, size, etag, content_type, metadata`,
		bucketID, key).Scan(&removed.BlobDigest, &removed.Size, &removed.ETag,
		&removed.ContentType, &previousMetadataHolder{&removed})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, fmt.Errorf("delete object %q: %w", key, err)
	}

	if opts.Versioning {
		// The deleted state is kept as a version, and a delete marker records
		// that the key was removed at this point. The bytes stay referenced by
		// the version, so the space is not reclaimed until it is purged.
		superseded := &ObjectVersion{
			BucketID: bucketID, Key: key, BlobDigest: &removed.BlobDigest,
			Size: removed.Size, ETag: removed.ETag, ContentType: removed.ContentType,
			Metadata: removed.Metadata, CreatedBy: opts.Actor,
		}
		if err := RecordVersion(ctx, tx, superseded); err != nil {
			return false, err
		}
		marker := &ObjectVersion{
			BucketID: bucketID, Key: key, IsDeleteMarker: true, CreatedBy: opts.Actor,
		}
		if err := RecordVersion(ctx, tx, marker); err != nil {
			return false, err
		}
	}

	if _, err := ReleaseBlob(ctx, tx, removed.BlobDigest); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit delete object: %w", err)
	}
	return true, nil
}

// previousMetadataHolder decodes a JSONB metadata column straight into an
// Object while scanning, so reading the superseded row needs one round trip
// rather than a scan followed by a separate decode.
type previousMetadataHolder struct{ target *Object }

func (h previousMetadataHolder) ScanBytes(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, &h.target.Metadata)
}

// Scan satisfies sql.Scanner, which is the interface pgx reaches for when a
// destination is not one of its known types.
func (h previousMetadataHolder) Scan(value any) error {
	switch typed := value.(type) {
	case nil:
		return nil
	case []byte:
		return h.ScanBytes(typed)
	case string:
		return h.ScanBytes([]byte(typed))
	}
	return fmt.Errorf("cannot decode metadata from %T", value)
}
