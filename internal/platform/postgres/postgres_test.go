package postgres_test

import (
	"os"
	"testing"
	"testing/fstest"

	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func TestConnectUsesUTC(t *testing.T) {
	pool := pgtest.NewPool(t)

	var zone string
	if err := pool.QueryRow(t.Context(), "SHOW timezone").Scan(&zone); err != nil {
		t.Fatal(err)
	}
	if zone != "UTC" {
		t.Errorf("timezone = %q, want UTC", zone)
	}
}

// Regression: pgx sizes a pool by the CPU count, which made the ledger's race tests
// pass on a 12-core laptop and fail on a 4-core CI runner. Test pools are fixed.
func TestTestPoolsDoNotDependOnTheMachine(t *testing.T) {
	if got := pgtest.NewPool(t).Config().MaxConns; got != pgtest.MaxConns {
		t.Errorf("test pool MaxConns = %d, want %d", got, pgtest.MaxConns)
	}
}

func TestConnectRejectsUnreachableDatabase(t *testing.T) {
	_, err := postgres.Connect(t.Context(), "postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err == nil {
		t.Fatal("Connect() error = nil, want error")
	}
}

func TestMigrateAppliesPendingOnce(t *testing.T) {
	pool := pgtest.NewEmptyPool(t)
	migrations := os.DirFS("testdata/migrations")

	applied, err := postgres.Migrate(t.Context(), pool, migrations)
	if err != nil {
		t.Fatalf("first Migrate() error = %v", err)
	}
	if applied != 2 {
		t.Errorf("first Migrate() applied %d, want 2", applied)
	}

	applied, err = postgres.Migrate(t.Context(), pool, migrations)
	if err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
	if applied != 0 {
		t.Errorf("second Migrate() applied %d, want 0", applied)
	}

	if _, err := pool.Exec(t.Context(), "INSERT INTO widgets (id, name, color) VALUES (1, 'a', 'red')"); err != nil {
		t.Errorf("schema from migrations missing: %v", err)
	}
}

func TestMigrateWithoutMigrationsIsNoop(t *testing.T) {
	pool := pgtest.NewEmptyPool(t)

	applied, err := postgres.Migrate(t.Context(), pool, fstest.MapFS{"README.md": {Data: []byte("not a migration")}})
	if err != nil {
		t.Fatalf("Migrate() error = %v, want nil", err)
	}
	if applied != 0 {
		t.Errorf("Migrate() applied %d, want 0", applied)
	}
}
