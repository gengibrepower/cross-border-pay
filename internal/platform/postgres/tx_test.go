package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

func newCounter(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.NewPool(t)
	_, err := pool.Exec(t.Context(), `
		CREATE TABLE counters (id int PRIMARY KEY, n int NOT NULL);
		INSERT INTO counters (id, n) VALUES (1, 0);`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func counter(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.WithoutCancel(t.Context()), "SELECT n FROM counters WHERE id = 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func increment(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "UPDATE counters SET n = n + 1 WHERE id = 1")
	return err
}

func TestWithTxCommits(t *testing.T) {
	pool := newCounter(t)

	if err := postgres.WithTx(t.Context(), pool, pgx.ReadCommitted, increment); err != nil {
		t.Fatalf("WithTx() error = %v", err)
	}
	if got := counter(t, pool); got != 1 {
		t.Errorf("counter = %d, want 1", got)
	}
}

func TestWithTxRollsBackOnError(t *testing.T) {
	pool := newCounter(t)
	boom := errors.New("boom")
	attempts := 0

	err := postgres.WithTx(t.Context(), pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		attempts++
		if err := increment(ctx, tx); err != nil {
			return err
		}
		return boom
	})

	if !errors.Is(err, boom) {
		t.Errorf("WithTx() error = %v, want %v", err, boom)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: an ordinary error is not retried", attempts)
	}
	if got := counter(t, pool); got != 0 {
		t.Errorf("counter = %d, want 0 after rollback", got)
	}
}

func TestWithTxRollsBackAndRepanics(t *testing.T) {
	pool := newCounter(t)
	defer func() {
		if r := recover(); r != "boom" {
			t.Errorf("recover() = %v, want the original panic \"boom\"", r)
		}
		if got := counter(t, pool); got != 0 {
			t.Errorf("counter = %d, want 0 after rollback", got)
		}
	}()

	_ = postgres.WithTx(t.Context(), pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		if err := increment(ctx, tx); err != nil {
			return err
		}
		panic("boom")
	})
	t.Error("WithTx returned instead of panicking")
}

func TestWithTxRetriesSerializationFailure(t *testing.T) {
	pool := newCounter(t)
	attempts := 0

	err := postgres.WithTx(t.Context(), pool, pgx.Serializable, func(ctx context.Context, tx pgx.Tx) error {
		attempts++
		var n int
		if err := tx.QueryRow(ctx, "SELECT n FROM counters WHERE id = 1").Scan(&n); err != nil {
			return err
		}
		if attempts == 1 {
			// A concurrent writer commits after this transaction took its snapshot.
			if _, err := pool.Exec(ctx, "UPDATE counters SET n = n + 1 WHERE id = 1"); err != nil {
				return err
			}
		}
		return increment(ctx, tx)
	})
	if err != nil {
		t.Fatalf("WithTx() error = %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if got := counter(t, pool); got != 2 {
		t.Errorf("counter = %d, want 2", got)
	}
}

func TestWithTxGivesUpAfterMaxAttempts(t *testing.T) {
	pool := newCounter(t)
	attempts := 0

	err := postgres.WithTx(t.Context(), pool, pgx.ReadCommitted, func(context.Context, pgx.Tx) error {
		attempts++
		return &pgconn.PgError{Code: "40001"}
	})

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
		t.Errorf("WithTx() error = %v, want a wrapped 40001", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}
