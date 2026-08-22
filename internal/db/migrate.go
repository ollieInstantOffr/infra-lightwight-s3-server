package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is an arbitrary but fixed key for a Postgres session-level
// advisory lock. It serialises migration across concurrent starts, so two
// containers coming up together cannot both try to apply the same DDL.
const migrationLockID int64 = 0x5333_44_01 // "S3D" + version

type migration struct {
	version  int
	name     string
	sql      string
	checksum []byte
}

// Migrate applies every pending migration in order. It is safe to call on every
// startup: already-applied migrations are skipped, and re-running with nothing
// pending is a no-op.
func Migrate(ctx context.Context, pool *Pool, log *slog.Logger) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	// A dedicated connection is required: advisory locks are held per session,
	// so acquiring and releasing must happen on the same connection.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
			log.Warn("could not release migration lock; it clears when the connection closes", "error", err)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INT         PRIMARY KEY,
			name       TEXT        NOT NULL,
			checksum   BYTEA       NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, conn.Conn())
	if err != nil {
		return err
	}

	pending := 0
	for _, m := range migrations {
		if prior, ok := applied[m.version]; ok {
			// An edited migration means the database and the source tree have
			// silently diverged. Refusing here is far kinder than letting the
			// mismatch surface as a confusing runtime error later.
			if !equalChecksum(prior, m.checksum) {
				return fmt.Errorf(
					"migration %04d_%s has changed since it was applied; "+
						"migrations are immutable once applied, add a new one instead",
					m.version, m.name)
			}
			continue
		}

		if err := applyMigration(ctx, conn.Conn(), m); err != nil {
			return err
		}
		log.Info("applied migration", "version", m.version, "name", m.name)
		pending++
	}

	if pending == 0 {
		log.Info("database schema up to date", "version", migrations[len(migrations)-1].version)
	} else {
		log.Info("database migrated", "applied", pending, "version", migrations[len(migrations)-1].version)
	}
	return nil
}

// applyMigration runs one migration and records it in the same transaction, so
// a failure part-way through leaves neither the DDL nor the bookkeeping behind.
func applyMigration(ctx context.Context, conn *pgx.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %04d: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.version, m.name, m.checksum); err != nil {
		return fmt.Errorf("record migration %04d: %w", m.version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %04d: %w", m.version, err)
	}
	return nil
}

func appliedMigrations(ctx context.Context, conn *pgx.Conn) (map[int][]byte, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int][]byte)
	for rows.Next() {
		var version int
		var checksum []byte
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = checksum
	}
	return applied, rows.Err()
}

// loadMigrations reads the embedded .sql files and returns them ordered by
// version. Filenames must be NNNN_description.sql.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	var out []migration
	seen := make(map[int]string)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, err
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %04d: %q and %q", version, other, entry.Name())
		}
		seen[version] = entry.Name()

		body, err := fs.ReadFile(migrationFS, path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{version: version, name: name, sql: string(body), checksum: sum[:]})
	}

	if len(out) == 0 {
		return nil, errors.New("no migrations found; the embedded migrations directory is empty")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	prefix, name, found := strings.Cut(base, "_")
	if !found || name == "" {
		return 0, "", fmt.Errorf("migration %q must be named NNNN_description.sql", filename)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("migration %q has a non-numeric version prefix", filename)
	}
	return version, name, nil
}

// equalChecksum compares without allocating; these are never secret, so a plain
// comparison is fine.
func equalChecksum(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
