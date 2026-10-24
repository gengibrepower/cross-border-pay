package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/migrations"
)

const (
	image        = "postgres:18.6-alpine"
	templateName = "migrated_template"
)

var (
	adminURL  *url.URL
	adminPool *pgxpool.Pool
	cloning   sync.Mutex
	sequence  atomic.Int64
)

// Main starts one Postgres container for the test binary and builds a template
// database with every migration applied. Use it as
// func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }.
func Main(m *testing.M) int {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	defer func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			fmt.Fprintln(os.Stderr, "pgtest: terminate container:", err)
		}
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgtest: start container:", err)
		return 1
	}

	if err := prepare(ctx, container); err != nil {
		fmt.Fprintln(os.Stderr, "pgtest:", err)
		return 1
	}
	defer adminPool.Close()
	return m.Run()
}

func prepare(ctx context.Context, container *tcpostgres.PostgresContainer) error {
	raw, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return fmt.Errorf("connection string: %w", err)
	}
	if adminURL, err = url.Parse(raw); err != nil {
		return fmt.Errorf("parse connection string: %w", err)
	}
	if adminPool, err = postgres.Connect(ctx, raw); err != nil {
		return fmt.Errorf("admin connection: %w", err)
	}

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quote(templateName)); err != nil {
		return fmt.Errorf("create template: %w", err)
	}
	template, err := postgres.Connect(ctx, databaseURL(templateName))
	if err != nil {
		return fmt.Errorf("template connection: %w", err)
	}
	defer template.Close()
	if _, err := postgres.Migrate(ctx, template, migrations.FS); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}
	return nil
}

// NewDatabase clones the migrated template into a database only this test sees,
// dropped when the test ends, and returns its connection URL.
func NewDatabase(t testing.TB) string {
	t.Helper()
	return newDatabase(t, " TEMPLATE "+quote(templateName))
}

// NewEmptyPool connects to a database without the project's migrations, for tests of
// the migration machinery itself.
func NewEmptyPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return connect(t, newDatabase(t, ""))
}

func newDatabase(t testing.TB, template string) string {
	t.Helper()
	if adminPool == nil {
		t.Fatal("pgtest: call pgtest.Main from this package's TestMain")
	}
	name := fmt.Sprintf("test_%d", sequence.Add(1))

	cloning.Lock()
	_, err := adminPool.Exec(t.Context(), "CREATE DATABASE "+quote(name)+template)
	cloning.Unlock()
	if err != nil {
		t.Fatalf("pgtest: create database: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)"); err != nil {
			t.Errorf("pgtest: drop database: %v", err)
		}
	})
	return databaseURL(name)
}

func NewPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return connect(t, NewDatabase(t))
}

func connect(t testing.TB, databaseURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func databaseURL(name string) string {
	u := *adminURL
	u.Path = "/" + name
	return u.String()
}

func quote(identifier string) string {
	return pgx.Identifier{identifier}.Sanitize()
}
