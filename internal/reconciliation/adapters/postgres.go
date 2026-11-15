package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/publicid"

	"github.com/gengibrepower/cross-border-pay/internal/reconciliation/adapters/reconciliationdb"
	"github.com/gengibrepower/cross-border-pay/internal/reconciliation/app"
	"github.com/gengibrepower/cross-border-pay/internal/reconciliation/domain"
)

// Reviews keeps no state: Open runs in the caller's transaction, so the case commits
// with the posting it names or not at all.
type Reviews struct{}

// Open: a charge seen again is not an error, so the caller can skip what it already did.
func (Reviews) Open(ctx context.Context, tx pgx.Tx, c *domain.Case) (opened bool, err error) {
	_, err = reconciliationdb.New(tx).OpenCase(ctx, reconciliationdb.OpenCaseParams{
		Kind:            string(c.Kind),
		Provider:        c.Provider,
		ExternalID:      c.ExternalID,
		ProviderKey:     pgtype.Text{String: c.ProviderKey, Valid: c.ProviderKey != ""},
		PaymentID:       optionalUUID(c.Payment),
		AttemptID:       optionalUUID(c.Attempt),
		Amount:          c.Amount.Amount(),
		Currency:        c.Amount.Currency().Code(),
		LedgerReference: pgtype.Text{String: c.LedgerReference, Valid: c.LedgerReference != ""},
		Details:         c.Details,
		CreatedAt:       pgtype.Timestamptz{Time: c.CreatedAt, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reconciliation: open %s case for %s %s: %w", c.Kind, c.Provider, c.ExternalID, err)
	}
	return true, nil
}

// Cases is the review queue as the reconciliation sees it, outside any payment's
// transaction.
type Cases struct {
	pool *pgxpool.Pool
}

func NewCases(pool *pgxpool.Pool) *Cases { return &Cases{pool: pool} }

func (c *Cases) Open(ctx context.Context, cs *domain.Case) (opened bool, err error) {
	err = pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		opened, err = Reviews{}.Open(ctx, tx, cs)
		return err
	})
	return opened, err
}

// Held is the money cases hold, as movements the gateway received: keyed by the
// attempt, or by what the provider named when no attempt has it.
func (c *Cases) Held(ctx context.Context, provider string, since, until time.Time) ([]domain.Line, error) {
	rows, err := reconciliationdb.New(c.pool).HeldBetween(ctx, reconciliationdb.HeldBetweenParams{
		Provider: provider, FromAt: pgtype.Timestamptz{Time: since, Valid: true}, ToAt: pgtype.Timestamptz{Time: until, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("reconciliation: held by %s: %w", provider, err)
	}
	out := make([]domain.Line, 0, len(rows))
	for _, r := range rows {
		cur, err := money.ParseCurrency(r.Currency)
		if err != nil {
			return nil, fmt.Errorf("reconciliation: case of %s: %w", r.ExternalID, err)
		}
		key := r.ProviderKey.String
		if r.AttemptID.Valid {
			key = publicid.Encode("att_", r.AttemptID.Bytes)
		}
		out = append(out, domain.Line{Kind: domain.Received, ExternalID: r.ExternalID, Key: key, Amount: money.New(r.Amount, cur), At: r.CreatedAt.Time})
	}
	return out, nil
}

// Runs are the daily reconciliations, one per provider and day.
type Runs struct {
	pool *pgxpool.Pool
}

func NewRuns(pool *pgxpool.Pool) *Runs { return &Runs{pool: pool} }

func (r *Runs) Claim(ctx context.Context, provider string, day calendar.Date, now, leaseUntil time.Time) (bool, error) {
	_, err := reconciliationdb.New(r.pool).ClaimRun(ctx, reconciliationdb.ClaimRunParams{
		Provider: provider, Day: dateOf(day),
		LeaseUntil: pgtype.Timestamptz{Time: leaseUntil, Valid: true}, Now: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reconciliation: claim %s %s: %w", provider, day, err)
	}
	return true, nil
}

func (r *Runs) Finish(ctx context.Context, provider string, day calendar.Date, rep *app.Report, now time.Time) error {
	err := reconciliationdb.New(r.pool).FinishRun(ctx, reconciliationdb.FinishRunParams{
		Provider: provider, Day: dateOf(day), FinishedAt: pgtype.Timestamptz{Time: now, Valid: true},
		Divergences: int32(rep.Divergences), Settled: int32(rep.Settled), Opened: int32(rep.Opened), //nolint:gosec // a day's counts fit
	})
	if err != nil {
		return fmt.Errorf("reconciliation: finish %s %s: %w", provider, day, err)
	}
	return nil
}

func dateOf(d calendar.Date) pgtype.Date { return pgtype.Date{Time: d.StartIn(time.UTC), Valid: true} }

func optionalUUID(id *[16]byte) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}
