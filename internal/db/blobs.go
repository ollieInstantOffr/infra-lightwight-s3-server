package db

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the subset of pgx that both *pgxpool.Pool and pgx.Tx satisfy.
// Taking it rather than a concrete type lets each helper run inside the
// caller's transaction, which is what makes reference counting correct: the
// count changes atomically with the object row that caused the change.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// BlobRemover unlinks a blob's file. *storage.Store satisfies it; taking an
// interface keeps this package free of a dependency on the filesystem layer.
type BlobRemover interface {
	Remove(digest string) error
}

// blobLockID derives a stable advisory-lock key from a digest.
//
// This lock is what makes garbage collection safe. Without it there is a window
// where the sweeper has decided a blob is unreferenced but has not yet unlinked
// it, during which a new upload of identical content can dedupe onto the file
// and take a reference — leaving a live object pointing at a file the sweeper
// is about to delete. Retention and collection take the same lock, so the two
// can never interleave inside that window.
func blobLockID(digest string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(digest))
	return int64(h.Sum64())
}

// RetainBlob records a new reference to a blob, inserting the row if this is
// the first. The bytes must already be on disk: a row here asserts the file
// exists.
//
// Must be called inside a transaction — it takes a transaction-scoped advisory
// lock that is only released on commit or rollback.
func RetainBlob(ctx context.Context, q Querier, digest string, size int64) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, blobLockID(digest)); err != nil {
		return fmt.Errorf("lock blob %s: %w", digest, err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO blobs (digest, size, refcount)
		VALUES ($1, $2, 1)
		ON CONFLICT (digest) DO UPDATE SET refcount = blobs.refcount + 1`,
		digest, size); err != nil {
		return fmt.Errorf("retain blob %s: %w", digest, err)
	}
	return nil
}

// ReleaseBlob drops a reference and reports how many remain. A zero result
// means the blob is now garbage, but nothing is unlinked here: deleting the
// file inside the caller's transaction would destroy data that a rollback was
// about to make live again. SweepUnreferenced does the deleting, later.
func ReleaseBlob(ctx context.Context, q Querier, digest string) (remaining int64, err error) {
	err = q.QueryRow(ctx, `
		UPDATE blobs
		SET refcount = GREATEST(refcount - 1, 0)
		WHERE digest = $1
		RETURNING refcount`, digest).Scan(&remaining)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing to release; a retried delete is not an error.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("release blob %s: %w", digest, err)
	}
	return remaining, nil
}

// SweepUnreferenced deletes blobs that nothing references any more, returning
// how many were reclaimed.
//
// grace is how long a blob must have sat unreferenced before it is eligible. It
// exists because a freshly written blob is momentarily at zero references,
// between the bytes landing and the object row committing.
//
// Each blob is claimed in its own transaction holding that blob's advisory
// lock, and the file is unlinked before the transaction commits. A concurrent
// upload of the same content therefore either completes first — in which case
// the refcount is no longer zero and the blob is skipped — or waits, and finds
// the row gone and writes the file afresh.
func SweepUnreferenced(
	ctx context.Context,
	pool *Pool,
	files BlobRemover,
	grace time.Duration,
	limit int,
	log *slog.Logger,
) (reclaimed int, err error) {
	candidates, err := unreferencedBlobs(ctx, pool, grace, limit)
	if err != nil {
		return 0, err
	}

	for _, digest := range candidates {
		claimed, err := claimAndUnlink(ctx, pool, files, digest)
		if err != nil {
			// One bad blob should not stop the sweep; log it and continue, and
			// it will be retried on the next pass.
			log.Warn("could not reclaim blob", "digest", digest, "error", err)
			continue
		}
		if claimed {
			reclaimed++
		}
	}
	return reclaimed, nil
}

func claimAndUnlink(ctx context.Context, pool *Pool, files BlobRemover, digest string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin sweep transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, blobLockID(digest)); err != nil {
		return false, fmt.Errorf("lock blob %s: %w", digest, err)
	}

	// Re-check under the lock: the blob may have been referenced again between
	// being listed and being claimed.
	tag, err := tx.Exec(ctx, `DELETE FROM blobs WHERE digest = $1 AND refcount = 0`, digest)
	if err != nil {
		return false, fmt.Errorf("claim blob %s: %w", digest, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	// Unlink before committing, while the lock still excludes any writer that
	// would otherwise dedupe onto this file.
	if err := files.Remove(digest); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit blob reclaim %s: %w", digest, err)
	}
	return true, nil
}

func unreferencedBlobs(ctx context.Context, q Querier, grace time.Duration, limit int) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT digest
		FROM blobs
		WHERE refcount = 0 AND created_at < now() - $1::interval
		ORDER BY created_at
		LIMIT $2`,
		grace.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("list unreferenced blobs: %w", err)
	}
	defer rows.Close()

	var digests []string
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return nil, fmt.Errorf("scan unreferenced blob: %w", err)
		}
		digests = append(digests, digest)
	}
	return digests, rows.Err()
}
