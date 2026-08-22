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

// PutObject writes or replaces an object.
//
// The blob must already be on disk. Reference counting is adjusted in the same
// transaction as the metadata: the new blob is retained, and any blob the key
// previously pointed at is released.
func PutObject(ctx context.Context, pool *Pool, obj *Object) error {
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
	var previousDigest string
	err = tx.QueryRow(ctx,
		`SELECT blob_digest FROM objects WHERE bucket_id = $1 AND key = $2`,
		obj.BucketID, obj.Key).Scan(&previousDigest)
	hadPrevious := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read existing object: %w", err)
	}

	if err := RetainBlob(ctx, tx, obj.BlobDigest, obj.Size); err != nil {
		return err
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
		if _, err := ReleaseBlob(ctx, tx, previousDigest); err != nil {
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
func DeleteObject(ctx context.Context, pool *Pool, bucketID, key string) (deleted bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin delete object: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`,
		objectLockID(bucketID, key)); err != nil {
		return false, fmt.Errorf("lock object key: %w", err)
	}

	var digest string
	err = tx.QueryRow(ctx,
		`DELETE FROM objects WHERE bucket_id = $1 AND key = $2 RETURNING blob_digest`,
		bucketID, key).Scan(&digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, fmt.Errorf("delete object %q: %w", key, err)
	}

	if _, err := ReleaseBlob(ctx, tx, digest); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit delete object: %w", err)
	}
	return true, nil
}
