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
	// VersionID identifies this state for as long as it exists, including after
	// it stops being current. Empty means the null version: the object was
	// written while its bucket was not versioned, and S3 reports it as "null".
	VersionID string
	CreatedAt time.Time
	UpdatedAt time.Time
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
	// Versioning keeps the superseded state as history rather than discarding
	// it. The old blob then stays referenced by its version, so the space is
	// not reclaimed until the versions are purged.
	Versioning VersioningState
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
	previous, err := readCurrent(ctx, tx, obj.BucketID, obj.Key)
	if err != nil {
		return err
	}
	previous.BucketID, previous.Key = obj.BucketID, obj.Key
	hadPrevious := previous.Exists

	if err := RetainBlob(ctx, tx, obj.BlobDigest, obj.Size); err != nil {
		return err
	}

	// The state being replaced is preserved as a version where the bucket's
	// versioning calls for it, taking its own reference to the old blob before
	// the live one is released below.
	obj.VersionID, err = supersede(ctx, tx, previous, opts)
	if err != nil {
		return err
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO objects (bucket_id, key, blob_digest, size, etag, content_type, metadata, version_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (bucket_id, key) DO UPDATE SET
			blob_digest  = EXCLUDED.blob_digest,
			size         = EXCLUDED.size,
			etag         = EXCLUDED.etag,
			content_type = EXCLUDED.content_type,
			metadata     = EXCLUDED.metadata,
			version_id   = EXCLUDED.version_id,
			updated_at   = now()
		RETURNING id::text, created_at, updated_at`,
		obj.BucketID, obj.Key, obj.BlobDigest, obj.Size, obj.ETag, obj.ContentType, metadata,
		obj.VersionID,
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
		SELECT id::text, blob_digest, size, etag, content_type, metadata,
		       version_id, created_at, updated_at
		FROM objects WHERE bucket_id = $1 AND key = $2`,
		bucketID, key,
	).Scan(&obj.ID, &obj.BlobDigest, &obj.Size, &obj.ETag, &obj.ContentType,
		&metadata, &obj.VersionID, &obj.CreatedAt, &obj.UpdatedAt)
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

// Deletion is what a delete reports back, because on a versioned bucket a
// delete is a write and the client needs to know what it wrote.
type Deletion struct {
	// Deleted is false only when the key did not exist on an unversioned
	// bucket. It is not an error either way: S3 makes DELETE idempotent and
	// clients rely on that when cleaning up.
	Deleted bool
	// MarkerVersionID identifies the delete marker, when one was created.
	// Clients read it from x-amz-version-id and use it to undo the delete.
	MarkerVersionID string
	// DeleteMarker says a marker was written rather than data removed, which
	// the client is told through x-amz-delete-marker.
	DeleteMarker bool
}

// DeleteObject removes an object, or marks it deleted on a versioned bucket.
//
// On an unversioned bucket this releases the blob and the space comes back. On
// a versioned one nothing is removed at all: the state that was current is
// preserved and a delete marker becomes current, so a plain GET returns 404
// while the bytes are still there, and deleting the marker by its own id brings
// the object back. That last behaviour has no counterpart in a plain delete and
// is the whole reason versioned deletes are worth having.
func DeleteObject(ctx context.Context, pool *Pool, bucketID, key string, opts WriteOptions) (Deletion, error) {
	var result Deletion

	tx, err := pool.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("begin delete object: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`,
		objectLockID(bucketID, key)); err != nil {
		return result, fmt.Errorf("lock object key: %w", err)
	}

	removed, err := readCurrent(ctx, tx, bucketID, key)
	if err != nil {
		return result, err
	}
	removed.BucketID, removed.Key = bucketID, key

	if opts.Versioning.Configured() {
		// The state that was current is preserved before the marker is written.
		//
		// A delete marker on a key that never existed is still a delete marker:
		// S3 creates one and reports success, which is why this runs before the
		// not-found check below rather than after it.
		if _, err := supersede(ctx, tx, removed, opts); err != nil {
			return result, err
		}

		markerID := NullVersionID
		if opts.Versioning == VersioningEnabled {
			if markerID, err = NewVersionID(); err != nil {
				return result, err
			}
		} else if err := replaceNullVersion(ctx, tx, bucketID, key); err != nil {
			// A suspended bucket's marker is the null version, and there can
			// only be one, so any existing null version gives way to it.
			return result, err
		}

		marker := &ObjectVersion{
			BucketID: bucketID, Key: key, VersionID: markerID,
			IsDeleteMarker: true, CreatedBy: opts.Actor,
		}
		if err := RecordVersion(ctx, tx, marker); err != nil {
			return result, err
		}
		result.DeleteMarker = true
		result.MarkerVersionID = externalVersionID(marker.VersionID)
	}

	if !removed.Exists {
		// Nothing was current. On a versioned bucket a marker was still written
		// above, which S3 treats as a successful delete.
		result.Deleted = opts.Versioning.Configured()
		return result, tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM objects WHERE bucket_id = $1 AND key = $2`,
		bucketID, key); err != nil {
		return result, fmt.Errorf("delete object %q: %w", key, err)
	}

	// The objects row's own reference goes either way. Where the state was
	// preserved as a version, that version took a reference first and the bytes
	// survive; where it was not, this is the last reference and the space is
	// reclaimed.
	if _, err := ReleaseBlob(ctx, tx, removed.BlobDigest); err != nil {
		return result, err
	}
	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit delete object: %w", err)
	}
	result.Deleted = true
	return result, nil
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
