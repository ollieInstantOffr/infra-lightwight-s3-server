package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The rules for what happens to the version a write replaces.
//
// Three call sites replace a key — a plain PUT, a completed multipart upload,
// and a restore — and they must all agree. They did agree by being written the
// same way three times, which is exactly the arrangement that drifts the first
// time one of them is edited. The rules live here instead.
//
// Two invariants hold everywhere:
//
//   objects holds the current version; object_versions holds the rest.
//   A version id never changes. When a state stops being current it moves into
//   object_versions carrying the id it already had.
//
// The second is what makes a version id worth handing to a client at all.

// currentState is the version of a key that a write is about to replace.
type currentState struct {
	Object
	VersionID string
	Exists    bool
}

// readCurrent reads the current version of a key. The caller must already hold
// the key's advisory lock, so what it returns is stable for the transaction.
func readCurrent(ctx context.Context, q Querier, bucketID, key string) (currentState, error) {
	var cur currentState
	err := q.QueryRow(ctx, `
		SELECT blob_digest, size, etag, content_type, metadata, version_id, updated_at
		FROM objects WHERE bucket_id = $1 AND key = $2`,
		bucketID, key,
	).Scan(&cur.BlobDigest, &cur.Size, &cur.ETag, &cur.ContentType,
		&previousMetadataHolder{&cur.Object}, &cur.VersionID, &cur.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return cur, nil
	}
	if err != nil {
		return cur, fmt.Errorf("read current version: %w", err)
	}
	cur.Exists = true
	return cur, nil
}

// supersede moves the current version into history where the bucket's
// versioning requires it, and returns the id the incoming version should carry.
//
// It does not release the old blob. The caller releases the objects row's
// reference as it always did; where the state was preserved, the version row
// has taken a reference of its own first, so the bytes survive.
func supersede(ctx context.Context, q Querier, cur currentState, opts WriteOptions) (string, error) {
	preserve := false
	switch opts.Versioning {
	case VersioningEnabled:
		preserve = cur.Exists

	case VersioningSuspended:
		// Only a version with a real id is preserved. The null version is
		// replaced outright, which is the one genuinely surprising rule in S3
		// versioning and the reason suspending is not a safe way to pause
		// history: write twice to a suspended bucket and the intermediate state
		// is gone, while everything written while it was enabled survives.
		preserve = cur.Exists && cur.VersionID != ""
	}

	if preserve {
		superseded := &ObjectVersion{
			BucketID: cur.BucketID, Key: cur.Key, VersionID: cur.VersionID,
			BlobDigest: &cur.BlobDigest, Size: cur.Size, ETag: cur.ETag,
			ContentType: cur.ContentType, Metadata: cur.Metadata, CreatedBy: opts.Actor,
			// The time the version was written, not the time it stopped being
			// current. A client reading LastModified wants to know when these
			// bytes were stored; that they were superseded on Tuesday is not a
			// property of the version.
			CreatedAt: cur.UpdatedAt,
		}
		// An id of "" means the null version, and RecordVersion would otherwise
		// mint a fresh one — which would silently change an id a client holds.
		if superseded.VersionID == "" {
			superseded.VersionID = NullVersionID
		}
		if err := RecordVersion(ctx, q, superseded); err != nil {
			return "", err
		}
	}

	// Only an enabled bucket mints new ids. Under suspension, and on a bucket
	// that was never versioned, the incoming state is the null version.
	if opts.Versioning == VersioningEnabled {
		return NewVersionID()
	}
	return "", nil
}

// replaceNullVersion removes any stored null version of a key.
//
// Needed when a suspended bucket writes a new null version — a delete marker,
// in practice — because there can only be one, and the unique constraint on
// (bucket, key, version) would otherwise reject the write rather than replace
// it. Releases the blob it held, since nothing points at that state any more.
func replaceNullVersion(ctx context.Context, q Querier, bucketID, key string) error {
	var digest *string
	err := q.QueryRow(ctx, `
		DELETE FROM object_versions
		WHERE bucket_id = $1 AND key = $2 AND version_id = $3
		RETURNING blob_digest`, bucketID, key, NullVersionID).Scan(&digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("replace null version: %w", err)
	}
	if digest != nil {
		if _, err := ReleaseBlob(ctx, q, *digest); err != nil {
			return err
		}
	}
	return nil
}
