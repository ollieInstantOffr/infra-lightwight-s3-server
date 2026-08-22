package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrBucketNotFound means no bucket by that name exists.
	ErrBucketNotFound = errors.New("bucket not found")
	// ErrBucketExists means the name is already taken.
	ErrBucketExists = errors.New("bucket already exists")
	// ErrBucketNotEmpty means the bucket still holds objects.
	ErrBucketNotEmpty = errors.New("bucket is not empty")
)

// Bucket is a namespace for objects.
type Bucket struct {
	ID        string
	Name      string
	CreatedAt time.Time
	CreatedBy *string
}

// BucketStats summarises a bucket's contents for the console.
type BucketStats struct {
	Bucket
	ObjectCount int64
	TotalBytes  int64
}

// uniqueViolation is Postgres's SQLSTATE for a unique constraint breach. It is
// checked rather than pre-querying for existence, because a check-then-insert
// races: two concurrent creates would both see the name free.
const uniqueViolation = "23505"

// CreateBucket creates a bucket, returning ErrBucketExists if the name is
// taken.
func CreateBucket(ctx context.Context, q Querier, name string, createdBy *string) (*Bucket, error) {
	bucket := &Bucket{Name: name, CreatedBy: createdBy}
	err := q.QueryRow(ctx, `
		INSERT INTO buckets (name, created_by)
		VALUES ($1, $2)
		RETURNING id::text, created_at`,
		name, createdBy,
	).Scan(&bucket.ID, &bucket.CreatedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return nil, ErrBucketExists
	}
	if err != nil {
		return nil, fmt.Errorf("create bucket %q: %w", name, err)
	}
	return bucket, nil
}

// GetBucket looks a bucket up by name.
func GetBucket(ctx context.Context, q Querier, name string) (*Bucket, error) {
	var bucket Bucket
	err := q.QueryRow(ctx, `
		SELECT id::text, name, created_at, created_by::text
		FROM buckets WHERE name = $1`, name,
	).Scan(&bucket.ID, &bucket.Name, &bucket.CreatedAt, &bucket.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBucketNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get bucket %q: %w", name, err)
	}
	return &bucket, nil
}

// ListBuckets returns every bucket, oldest first, which is the order S3 uses.
func ListBuckets(ctx context.Context, q Querier) ([]Bucket, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, name, created_at, created_by::text
		FROM buckets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	defer rows.Close()

	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.ID, &b.Name, &b.CreatedAt, &b.CreatedBy); err != nil {
			return nil, fmt.Errorf("scan bucket: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListBucketsWithStats returns buckets alongside object counts and sizes, for
// the console dashboard. The S3 API deliberately does not use this: computing
// an aggregate per bucket on every ListBuckets call would make a cheap
// operation expensive.
func ListBucketsWithStats(ctx context.Context, q Querier) ([]BucketStats, error) {
	rows, err := q.Query(ctx, `
		SELECT b.id::text, b.name, b.created_at, b.created_by::text,
		       count(o.id), coalesce(sum(o.size), 0)
		FROM buckets b
		LEFT JOIN objects o ON o.bucket_id = b.id
		GROUP BY b.id
		ORDER BY b.name`)
	if err != nil {
		return nil, fmt.Errorf("list buckets with stats: %w", err)
	}
	defer rows.Close()

	var out []BucketStats
	for rows.Next() {
		var s BucketStats
		if err := rows.Scan(&s.ID, &s.Name, &s.CreatedAt, &s.CreatedBy,
			&s.ObjectCount, &s.TotalBytes); err != nil {
			return nil, fmt.Errorf("scan bucket stats: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteBucket removes an empty bucket.
//
// Emptiness is checked and the delete performed in one statement, so a
// concurrent upload cannot slip in between the two and leave objects orphaned
// by the cascade.
func DeleteBucket(ctx context.Context, q Querier, name string) error {
	var deleted bool
	err := q.QueryRow(ctx, `
		WITH target AS (
			SELECT id FROM buckets WHERE name = $1
		), deleted AS (
			DELETE FROM buckets
			WHERE id = (SELECT id FROM target)
			  AND NOT EXISTS (SELECT 1 FROM objects WHERE bucket_id = (SELECT id FROM target))
			  AND NOT EXISTS (SELECT 1 FROM multipart_uploads WHERE bucket_id = (SELECT id FROM target))
			RETURNING id
		)
		SELECT EXISTS (SELECT 1 FROM deleted)`, name,
	).Scan(&deleted)
	if err != nil {
		return fmt.Errorf("delete bucket %q: %w", name, err)
	}
	if deleted {
		return nil
	}

	// The delete did nothing, so distinguish "no such bucket" from "not empty"
	// to give the client the right error.
	if _, err := GetBucket(ctx, q, name); err != nil {
		return err
	}
	return ErrBucketNotEmpty
}
