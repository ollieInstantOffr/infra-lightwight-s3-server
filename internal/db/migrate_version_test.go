package db

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// Migrations are forward-only, so redeploying an older binary over a newer
// schema is the realistic response to a bad release — and the one case where
// carrying on quietly does the most damage.

func TestSchemaAhead(t *testing.T) {
	cases := []struct {
		name    string
		applied []int
		newest  int
		want    int
	}{
		{"up to date", []int{1, 2, 3}, 3, 0},
		{"database behind the build", []int{1, 2}, 3, 0},
		{"database ahead", []int{1, 2, 3, 4}, 3, 4},
		{"several ahead reports the highest", []int{1, 2, 3, 4, 5}, 3, 5},
		{"empty database", nil, 3, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applied := map[int][]byte{}
			for _, v := range tc.applied {
				applied[v] = nil
			}
			if got := schemaAhead(applied, tc.newest); got != tc.want {
				t.Errorf("schemaAhead = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMigrateRefusesASchemaFromTheFuture(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Pretend a newer build has been here.
	if _, err := pool.Exec(ctx, `
		INSERT INTO schema_migrations (version, name, checksum)
		VALUES (9999, 'from_the_future', '\x00')`); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM schema_migrations WHERE version = 9999`)
	})

	err := Migrate(ctx, pool, quiet)
	if err == nil {
		t.Fatal("an older build started against a newer schema; it would read moved columns and write rows that no longer fit")
	}
	// The message has to say what to do, because whoever reads it is mid
	// rollback and the answer is not obvious.
	for _, want := range []string{"9999", "forward-only", "backup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestSchemaVersionMatchesWhatIsApplied(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	build, err := SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if build == 0 {
		t.Fatal("this build reports no migrations at all")
	}

	applied, err := AppliedSchemaVersion(ctx, pool)
	if err != nil {
		t.Fatalf("AppliedSchemaVersion: %v", err)
	}
	if applied != build {
		t.Errorf("database is at %d, build carries %d; the fixture migrates, so these should agree",
			applied, build)
	}
}
