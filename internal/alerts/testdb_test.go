package alerts

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// Go runs test packages in parallel, and every package that touches the
// database migrates and resets it. Sharing one schema means one package
// truncating tables out from under another, which shows up as unrelated
// foreign-key failures that move around between runs.
//
// Each package therefore gets its own Postgres schema inside the same test
// database: cheap to create, completely isolated, and no extra containers.
func testDSN(t *testing.T, schema string) string {
	t.Helper()

	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}

	// The schema has to exist before a connection can select it, so it is
	// created over a connection using the default search path.
	ctx := context.Background()
	admin, err := db.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to create test schema: %v", err)
	}
	defer admin.Close()

	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, schema)); err != nil {
		t.Fatalf("create test schema %q: %v", schema, err)
	}

	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	// pgx forwards unrecognised query parameters as server runtime settings, so
	// this scopes every statement on the connection to the package's schema.
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// testPool connects to this package's schema, migrates it, and clears the
// tables the alert tests touch.
func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := testDSN(t, "test_alerts_pkg")
	ctx := context.Background()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := db.Migrate(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{
		`DELETE FROM alerts`, `DELETE FROM alert_rules`,
		`DELETE FROM request_logs`, `DELETE FROM credentials`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}
	return pool
}
