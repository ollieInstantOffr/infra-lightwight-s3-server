package db

import (
	"context"
	"fmt"
)

// Advisory lock keys for work that must not run in two processes at once.
//
// Arbitrary but fixed, and kept together so a future one cannot accidentally
// collide with an existing one. They share a namespace with migrationLockID and
// the per-blob and per-object locks, which is why the high bytes differ.
const (
	// alertEngineLockID guards an alert evaluation cycle.
	alertEngineLockID int64 = 0x5333_44_02 // "S3D" + alerts
)

// WithSingleton runs fn only if no other process is already running it, and
// reports whether it ran.
//
// This exists for one reason: the alert engine sends a notification before
// marking it sent, so two engines evaluating at once both send. The operator
// gets two emails and there is no way to un-send the second.
//
// try-and-skip rather than block-and-wait is deliberate. A second engine that
// queued for the lock would run a full cycle the instant the first finished,
// which is the duplicate work the lock exists to prevent — just delayed by a
// few seconds. Skipping means the loser does nothing and tries again on its
// next tick, which is what a periodic task should do.
//
// The lock is session-scoped and taken on a dedicated connection, because a
// pooled one could be handed to another caller between the lock and the
// unlock. It is released when fn returns, and by Postgres itself if the
// process dies holding it — so a crashed engine does not wedge the others.
func WithSingleton(ctx context.Context, pool *Pool, key int64, fn func(context.Context) error) (ran bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection for singleton lock: %w", err)
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		return false, fmt.Errorf("take singleton lock: %w", err)
	}
	if !acquired {
		return false, nil
	}

	defer func() {
		// WithoutCancel: a cancelled context must still release the lock, or a
		// shutdown mid-cycle would hold it until the connection is reaped.
		if _, unlockErr := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, key); unlockErr != nil && err == nil {
			err = fmt.Errorf("release singleton lock: %w", unlockErr)
		}
	}()

	return true, fn(ctx)
}

// WithAlertEngineLock runs an alert evaluation cycle exclusively.
func WithAlertEngineLock(ctx context.Context, pool *Pool, fn func(context.Context) error) (bool, error) {
	return WithSingleton(ctx, pool, alertEngineLockID, fn)
}
