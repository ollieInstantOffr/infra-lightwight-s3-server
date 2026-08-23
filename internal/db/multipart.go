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

var (
	// ErrUploadNotFound means the upload id is unknown, aborted or completed.
	ErrUploadNotFound = errors.New("multipart upload not found")
	// ErrPartNotFound means a part referenced at completion was never uploaded.
	ErrPartNotFound = errors.New("multipart part not found")
)

// MultipartUpload is an in-progress multipart upload.
type MultipartUpload struct {
	ID          string
	UploadID    string
	BucketID    string
	Key         string
	ContentType string
	Metadata    map[string]string
	InitiatedAt time.Time
}

// MultipartPart is one uploaded part.
type MultipartPart struct {
	PartNumber int
	BlobDigest string
	Size       int64
	ETag       string
	UploadedAt time.Time
}

// NewUploadID returns an opaque upload identifier. It is deliberately not the
// row's UUID: the id is handed to clients and echoed back, and keeping it
// separate means the internal key can change without breaking in-flight
// uploads.
func NewUploadID() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate upload id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// CreateMultipartUpload records a new upload.
func CreateMultipartUpload(ctx context.Context, q Querier, upload *MultipartUpload) error {
	metadata, err := json.Marshal(upload.Metadata)
	if err != nil {
		return fmt.Errorf("encode upload metadata: %w", err)
	}
	if upload.UploadID == "" {
		if upload.UploadID, err = NewUploadID(); err != nil {
			return err
		}
	}

	err = q.QueryRow(ctx, `
		INSERT INTO multipart_uploads (upload_id, bucket_id, key, content_type, metadata)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, initiated_at`,
		upload.UploadID, upload.BucketID, upload.Key, upload.ContentType, metadata,
	).Scan(&upload.ID, &upload.InitiatedAt)
	if err != nil {
		return fmt.Errorf("create multipart upload: %w", err)
	}
	return nil
}

// GetMultipartUpload looks an upload up by its client-facing id.
//
// The bucket is matched too, so an upload id leaked from one bucket cannot be
// used to write into another.
func GetMultipartUpload(ctx context.Context, q Querier, bucketID, uploadID string) (*MultipartUpload, error) {
	upload := &MultipartUpload{UploadID: uploadID, BucketID: bucketID}
	var metadata []byte

	err := q.QueryRow(ctx, `
		SELECT id::text, key, content_type, metadata, initiated_at
		FROM multipart_uploads WHERE upload_id = $1 AND bucket_id = $2`,
		uploadID, bucketID,
	).Scan(&upload.ID, &upload.Key, &upload.ContentType, &metadata, &upload.InitiatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUploadNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get multipart upload: %w", err)
	}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &upload.Metadata); err != nil {
			return nil, fmt.Errorf("decode upload metadata: %w", err)
		}
	}
	return upload, nil
}

// PutMultipartPart records an uploaded part, replacing any previous upload of
// the same part number.
//
// S3 semantics are last-write-wins per part number, so re-uploading a part
// after a failed attempt is normal rather than an error. The superseded part's
// blob reference is released in the same transaction.
func PutMultipartPart(ctx context.Context, pool *Pool, uploadRowID string, part *MultipartPart) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin put part: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialised per part so two concurrent uploads of the same part number
	// cannot both believe they replaced the other, leaking a blob reference.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`,
		objectLockID(uploadRowID, fmt.Sprint(part.PartNumber))); err != nil {
		return fmt.Errorf("lock part: %w", err)
	}

	var previousDigest string
	err = tx.QueryRow(ctx,
		`SELECT blob_digest FROM multipart_parts WHERE upload_id = $1 AND part_number = $2`,
		uploadRowID, part.PartNumber).Scan(&previousDigest)
	hadPrevious := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read existing part: %w", err)
	}

	if err := RetainBlob(ctx, tx, part.BlobDigest, part.Size); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO multipart_parts (upload_id, part_number, blob_digest, size, etag)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (upload_id, part_number) DO UPDATE SET
			blob_digest = EXCLUDED.blob_digest,
			size        = EXCLUDED.size,
			etag        = EXCLUDED.etag,
			uploaded_at = now()`,
		uploadRowID, part.PartNumber, part.BlobDigest, part.Size, part.ETag); err != nil {
		return fmt.Errorf("write part %d: %w", part.PartNumber, err)
	}

	if hadPrevious {
		if _, err := ReleaseBlob(ctx, tx, previousDigest); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ListMultipartParts returns an upload's parts in ascending part order.
func ListMultipartParts(ctx context.Context, q Querier, uploadRowID string) ([]MultipartPart, error) {
	rows, err := q.Query(ctx, `
		SELECT part_number, blob_digest, size, etag, uploaded_at
		FROM multipart_parts WHERE upload_id = $1 ORDER BY part_number`, uploadRowID)
	if err != nil {
		return nil, fmt.Errorf("list parts: %w", err)
	}
	defer rows.Close()

	var out []MultipartPart
	for rows.Next() {
		var p MultipartPart
		if err := rows.Scan(&p.PartNumber, &p.BlobDigest, &p.Size, &p.ETag, &p.UploadedAt); err != nil {
			return nil, fmt.Errorf("scan part: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListMultipartUploads returns in-progress uploads in a bucket, ordered by key.
func ListMultipartUploads(ctx context.Context, q Querier, bucketID, keyMarker string, limit int) ([]MultipartUpload, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, upload_id, key, content_type, initiated_at
		FROM multipart_uploads
		WHERE bucket_id = $1 AND key > $2
		ORDER BY key, initiated_at
		LIMIT $3`, bucketID, keyMarker, limit)
	if err != nil {
		return nil, fmt.Errorf("list multipart uploads: %w", err)
	}
	defer rows.Close()

	var out []MultipartUpload
	for rows.Next() {
		u := MultipartUpload{BucketID: bucketID}
		if err := rows.Scan(&u.ID, &u.UploadID, &u.Key, &u.ContentType, &u.InitiatedAt); err != nil {
			return nil, fmt.Errorf("scan multipart upload: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CompleteMultipartUpload turns the assembled blob into an object and discards
// the upload.
//
// The concatenated blob must already be on disk; assembling bytes is the
// storage layer's job. This performs the metadata half atomically: the object
// row is written, the assembled blob retained, every part blob released, and
// the upload removed — so a failure leaves the upload intact and retryable
// rather than half-consumed.
func CompleteMultipartUpload(ctx context.Context, pool *Pool, upload *MultipartUpload, object *Object, opts WriteOptions) error {
	metadata, err := json.Marshal(object.Metadata)
	if err != nil {
		return fmt.Errorf("encode object metadata: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete upload: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`,
		objectLockID(object.BucketID, object.Key)); err != nil {
		return fmt.Errorf("lock object key: %w", err)
	}

	previous, err := readCurrent(ctx, tx, object.BucketID, object.Key)
	if err != nil {
		return err
	}
	previous.BucketID, previous.Key = object.BucketID, object.Key
	hadPrevious := previous.Exists

	if err := RetainBlob(ctx, tx, object.BlobDigest, object.Size); err != nil {
		return err
	}

	// A multipart completion replaces the key just as a plain PUT does, and
	// goes through the same rules so the two cannot diverge.
	object.VersionID, err = supersede(ctx, tx, previous, opts)
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
		object.BucketID, object.Key, object.BlobDigest, object.Size,
		object.ETag, object.ContentType, metadata, object.VersionID,
	).Scan(&object.ID, &object.CreatedAt, &object.UpdatedAt)
	if err != nil {
		return fmt.Errorf("write completed object: %w", err)
	}

	if hadPrevious {
		if _, err := ReleaseBlob(ctx, tx, previous.BlobDigest); err != nil {
			return err
		}
	}

	if err := releaseUpload(ctx, tx, upload.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AbortMultipartUpload discards an upload and releases every part's blob.
func AbortMultipartUpload(ctx context.Context, pool *Pool, uploadRowID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin abort upload: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := releaseUpload(ctx, tx, uploadRowID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// releaseUpload drops every part's blob reference and deletes the upload. The
// parts cascade from the upload row, so only the references need unwinding.
func releaseUpload(ctx context.Context, tx pgx.Tx, uploadRowID string) error {
	parts, err := ListMultipartParts(ctx, tx, uploadRowID)
	if err != nil {
		return err
	}
	for _, part := range parts {
		if _, err := ReleaseBlob(ctx, tx, part.BlobDigest); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM multipart_uploads WHERE id = $1`, uploadRowID); err != nil {
		return fmt.Errorf("delete multipart upload: %w", err)
	}
	return nil
}

// ReapAbandonedUploads discards uploads left in progress beyond maxAge.
//
// A client that starts a multipart upload and disappears leaves its parts
// referenced forever. Without this the disk fills with data no object will ever
// point at, and nothing surfaces the problem.
func ReapAbandonedUploads(ctx context.Context, pool *Pool, maxAge time.Duration, limit int) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text FROM multipart_uploads
		WHERE initiated_at < now() - $1::interval
		ORDER BY initiated_at
		LIMIT $2`, maxAge.String(), limit)
	if err != nil {
		return 0, fmt.Errorf("find abandoned uploads: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan abandoned upload: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	reaped := 0
	for _, id := range ids {
		if err := AbortMultipartUpload(ctx, pool, id); err != nil {
			return reaped, err
		}
		reaped++
	}
	return reaped, nil
}
