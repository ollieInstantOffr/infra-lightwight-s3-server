package db

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingRemover stands in for the filesystem so tests can observe exactly
// which blobs were unlinked, and inject a pause into the window that the
// advisory lock exists to protect.
type recordingRemover struct {
	mu      sync.Mutex
	removed []string
	onCall  func(digest string)
}

func (r *recordingRemover) Remove(digest string) error {
	if r.onCall != nil {
		r.onCall(digest)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, digest)
	return nil
}

func (r *recordingRemover) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.removed)
}

func testPool(t *testing.T) *Pool {
	t.Helper()
	dsn := testDSN(t, "test_db_pkg")
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := Migrate(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Ordered so foreign keys are satisfied: objects reference blobs.
	for _, stmt := range []string{
		`DELETE FROM buckets`, `DELETE FROM blobs`, `DELETE FROM credentials`,
		`DELETE FROM request_logs`, `DELETE FROM server_events`,
		`DELETE FROM alerts`, `DELETE FROM alert_rules`, `DELETE FROM request_metrics`,
		`DELETE FROM login_attempts`, `DELETE FROM sessions`, `DELETE FROM users`,
		`UPDATE app_settings SET resend_enabled = false, resend_from = '',
		 resend_api_key = NULL, resend_api_key_nonce = NULL WHERE id = true`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}
	return pool
}

// retain runs RetainBlob in its own transaction, as production callers do.
func retain(t *testing.T, pool *Pool, digest string, size int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := RetainBlob(ctx, tx, digest, size); err != nil {
		t.Fatalf("RetainBlob: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func digestOf(seed string) string {
	return strings.Repeat(seed, 64/len(seed))
}

func TestRefcountLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	digest := digestOf("ab")

	retain(t, pool, digest, 100)
	retain(t, pool, digest, 100)

	remaining, err := ReleaseBlob(ctx, pool, digest)
	if err != nil {
		t.Fatalf("ReleaseBlob: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("after one release refcount = %d, want 1", remaining)
	}

	remaining, err = ReleaseBlob(ctx, pool, digest)
	if err != nil {
		t.Fatalf("ReleaseBlob: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("after two releases refcount = %d, want 0", remaining)
	}

	// Releasing an unknown blob is a no-op, so a retried delete is safe.
	if _, err := ReleaseBlob(ctx, pool, digestOf("cd")); err != nil {
		t.Errorf("ReleaseBlob on unknown digest: %v", err)
	}
}

// Two objects sharing identical bytes must not delete each other's data.
func TestSweepLeavesStillReferencedBlobs(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	shared := digestOf("ab")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	retain(t, pool, shared, 42) // object A
	retain(t, pool, shared, 42) // object B, same bytes

	// Object A is deleted.
	if _, err := ReleaseBlob(ctx, pool, shared); err != nil {
		t.Fatalf("ReleaseBlob: %v", err)
	}

	files := &recordingRemover{}
	reclaimed, err := SweepUnreferenced(ctx, pool, files, 0, 100, log)
	if err != nil {
		t.Fatalf("SweepUnreferenced: %v", err)
	}
	if reclaimed != 0 || files.count() != 0 {
		t.Fatalf("sweep reclaimed %d blobs (%d unlinked) while object B still referenced them",
			reclaimed, files.count())
	}

	// Object B goes too; now it really is garbage.
	if _, err := ReleaseBlob(ctx, pool, shared); err != nil {
		t.Fatalf("ReleaseBlob: %v", err)
	}
	reclaimed, err = SweepUnreferenced(ctx, pool, files, 0, 100, log)
	if err != nil {
		t.Fatalf("SweepUnreferenced: %v", err)
	}
	if reclaimed != 1 || files.count() != 1 {
		t.Fatalf("reclaimed = %d, unlinked = %d; want 1 and 1", reclaimed, files.count())
	}
}

// A blob is briefly at zero references between its bytes landing and its object
// row committing. The grace period is what stops the sweeper deleting it then.
func TestSweepRespectsGracePeriod(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	digest := digestOf("ab")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := pool.Exec(ctx,
		`INSERT INTO blobs (digest, size, refcount) VALUES ($1, 10, 0)`, digest); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	files := &recordingRemover{}
	reclaimed, err := SweepUnreferenced(ctx, pool, files, time.Hour, 100, log)
	if err != nil {
		t.Fatalf("SweepUnreferenced: %v", err)
	}
	if reclaimed != 0 {
		t.Fatalf("reclaimed %d blobs inside the grace period, want 0", reclaimed)
	}

	reclaimed, err = SweepUnreferenced(ctx, pool, files, 0, 100, log)
	if err != nil {
		t.Fatalf("SweepUnreferenced: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed = %d once past the grace period, want 1", reclaimed)
	}
}

// The race the advisory lock exists to prevent: an upload of identical content
// arriving while the sweeper has claimed the blob but has not yet unlinked it.
// The retention must block until the sweep finishes, then insert a fresh row —
// never end up with a live reference to a file the sweeper went on to delete.
func TestSweepBlocksConcurrentRetention(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	digest := digestOf("ab")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := pool.Exec(ctx,
		`INSERT INTO blobs (digest, size, refcount) VALUES ($1, 10, 0)`, digest); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	retentionDone := make(chan error, 1)
	inWindow := make(chan struct{})

	files := &recordingRemover{
		onCall: func(string) {
			// The sweeper holds the lock here, mid-claim. Kick off a competing
			// retention and give it time to reach the lock and block.
			close(inWindow)
			go func() {
				tx, err := pool.Begin(ctx)
				if err != nil {
					retentionDone <- err
					return
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if err := RetainBlob(ctx, tx, digest, 10); err != nil {
					retentionDone <- err
					return
				}
				retentionDone <- tx.Commit(ctx)
			}()
			time.Sleep(250 * time.Millisecond)
		},
	}

	reclaimed, err := SweepUnreferenced(ctx, pool, files, 0, 100, log)
	if err != nil {
		t.Fatalf("SweepUnreferenced: %v", err)
	}
	<-inWindow
	if reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1", reclaimed)
	}

	select {
	case err := <-retentionDone:
		if err != nil {
			t.Fatalf("concurrent retention: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent retention never completed; the advisory lock may be deadlocked")
	}

	// The retention was serialised after the sweep, so it must have created a
	// brand new row at refcount 1 rather than resurrecting the claimed one.
	var refcount int64
	err = pool.QueryRow(ctx, `SELECT refcount FROM blobs WHERE digest = $1`, digest).Scan(&refcount)
	if err != nil {
		t.Fatalf("read refcount after race: %v", err)
	}
	if refcount != 1 {
		t.Errorf("refcount after race = %d, want 1", refcount)
	}
}

func TestRetainBlobRollsBackCleanly(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	digest := digestOf("ab")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := RetainBlob(ctx, tx, digest, 10); err != nil {
		t.Fatalf("RetainBlob: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE digest = $1`, digest).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("rolled-back retention left %d rows behind", count)
	}

	// The advisory lock must have been released by the rollback.
	done := make(chan error, 1)
	go func() {
		tx2, err := pool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = tx2.Rollback(ctx) }()
		done <- RetainBlob(ctx, tx2, digest, 10)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("retention after rollback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("advisory lock was not released by rollback")
	}
}
