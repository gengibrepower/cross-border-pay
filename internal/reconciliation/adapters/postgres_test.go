package adapters_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
	"github.com/gengibrepower/cross-border-pay/internal/reconciliation/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/reconciliation/domain"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func lateCase(reference string) *domain.Case {
	return &domain.Case{
		Kind: domain.LatePayment, Provider: "pixsim", ExternalID: "chg_1", Amount: money.New(10150, money.BRL),
		LedgerReference: reference, Details: "paid after the quote expired",
		CreatedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
	}
}

func TestOneCasePerCharge(t *testing.T) {
	pool := pgtest.NewPool(t)
	var opened []bool
	for range 2 {
		err := postgres.WithTx(t.Context(), pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
			ok, err := adapters.Reviews{}.Open(ctx, tx, lateCase("divergence:pixsim:chg_1"))
			if err != nil {
				return err
			}
			opened = append(opened, ok)
			if !ok {
				return nil
			}
			_, err = tx.Exec(ctx, "INSERT INTO ledger_transactions (reference) VALUES ('divergence:pixsim:chg_1')")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(opened) != 2 || !opened[0] || opened[1] {
		t.Errorf("Open() twice = %v, want [true false]", opened)
	}
}

// The case is written before its posting, so its reference is checked at commit: a
// case whose posting never came does not commit.
func TestACaseNeedsItsPosting(t *testing.T) {
	pool := pgtest.NewPool(t)
	err := postgres.WithTx(t.Context(), pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		_, err := adapters.Reviews{}.Open(ctx, tx, lateCase("divergence:pixsim:never-posted"))
		return err
	})
	if err == nil {
		t.Fatal("committed a case without its posting")
	}
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM review_cases").Scan(&n); err != nil || n != 0 {
		t.Errorf("%d cases (%v), want none", n, err)
	}
}
