// Package db owns the Postgres connection pool and the schema migrations.
//
// Object bytes are never stored here — those live on disk under DATA_DIR.
// Postgres holds only metadata: who may log in, which credentials exist, and
// what objects a bucket contains.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps pgxpool with the settings this server wants everywhere.
type Pool = pgxpool.Pool

const (
	// A single-node server does not need a large pool, and a small one fails
	// fast and visibly rather than queueing behind an exhausted database.
	maxConns = 16
	minConns = 2

	// Connections are recycled well before any proxy or firewall is likely to
	// drop them out from under us.
	maxConnLifetime = 30 * time.Minute
	maxConnIdleTime = 5 * time.Minute

	connectTimeout = 10 * time.Second
)

// Connect opens the pool and proves the database is actually reachable. It
// returns only once a real round-trip has succeeded, so a caller that gets a
// pool back can rely on it.
func Connect(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	cfg.MaxConns = maxConns
	cfg.MinConns = minConns
	cfg.MaxConnLifetime = maxConnLifetime
	cfg.MaxConnIdleTime = maxConnIdleTime
	cfg.ConnConfig.ConnectTimeout = connectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	return pool, nil
}
