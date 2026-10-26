package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

const (
	uniqueViolation = "23505"
	oneLiveAttempt  = "payment_attempts_one_live"
)

// CreateAttempt stores the intention to call the provider, in created. It returns
// app.ErrLiveAttempt while the payment has an attempt that was not rejected.
func (p *Postgres) CreateAttempt(ctx context.Context, payment domain.PaymentID, provider domain.ProviderName, now time.Time) (domain.PaymentAttempt, error) {
	row, err := paymentsdb.New(p.pool).InsertAttempt(ctx, paymentsdb.InsertAttemptParams{
		PaymentID: pgtype.UUID{Bytes: payment, Valid: true},
		Provider:  string(provider),
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == oneLiveAttempt {
		return domain.PaymentAttempt{}, fmt.Errorf("%w: %s", app.ErrLiveAttempt, payment)
	}
	if err != nil {
		return domain.PaymentAttempt{}, fmt.Errorf("payments: insert attempt: %w", err)
	}
	return restoreAttempt(&row)
}

func (p *Postgres) GetAttempt(ctx context.Context, id domain.AttemptID) (domain.PaymentAttempt, error) {
	row, err := paymentsdb.New(p.pool).GetAttempt(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PaymentAttempt{}, fmt.Errorf("%w: %s", app.ErrAttemptNotFound, id)
	}
	if err != nil {
		return domain.PaymentAttempt{}, fmt.Errorf("payments: get attempt: %w", err)
	}
	return restoreAttempt(&row)
}

// ApplyAttempt locks the attempt, moves it by the event through the domain's table and
// stores the result. The row lock serializes concurrent events: of two that race from
// the same status, the second sees the first one's result. changed is false for a
// duplicate, which writes nothing.
func (p *Postgres) ApplyAttempt(ctx context.Context, id domain.AttemptID, e domain.AttemptEvent, externalID string, now time.Time) (attempt domain.PaymentAttempt, changed bool, err error) {
	err = postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		attempt, changed, err = applyAttempt(ctx, paymentsdb.New(tx), id, e, externalID, now)
		return err
	})
	if err != nil {
		return domain.PaymentAttempt{}, false, fmt.Errorf("payments: apply %s to attempt %s: %w", e, id, err)
	}
	return attempt, changed, nil
}

func applyAttempt(ctx context.Context, q *paymentsdb.Queries, id domain.AttemptID, e domain.AttemptEvent, externalID string, now time.Time) (domain.PaymentAttempt, bool, error) {
	row, err := q.LockAttempt(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PaymentAttempt{}, false, app.ErrAttemptNotFound
	}
	if err != nil {
		return domain.PaymentAttempt{}, false, fmt.Errorf("lock: %w", err)
	}
	attempt, err := restoreAttempt(&row)
	if err != nil {
		return domain.PaymentAttempt{}, false, err
	}
	changed, err := attempt.Apply(e, externalID, now)
	if err != nil {
		return domain.PaymentAttempt{}, false, fmt.Errorf("apply: %w", err)
	}
	if !changed {
		return attempt, false, nil
	}
	err = q.UpdateAttempt(ctx, paymentsdb.UpdateAttemptParams{
		ID:         row.ID,
		Status:     string(attempt.Status()),
		ExternalID: pgtype.Text{String: attempt.ExternalID(), Valid: attempt.ExternalID() != ""},
		UpdatedAt:  pgtype.Timestamptz{Time: attempt.UpdatedAt(), Valid: true},
	})
	if err != nil {
		return domain.PaymentAttempt{}, false, fmt.Errorf("update: %w", err)
	}
	return attempt, true, nil
}

func restoreAttempt(row *paymentsdb.PaymentAttempt) (domain.PaymentAttempt, error) {
	status, err := domain.ParseAttemptStatus(row.Status)
	if err != nil {
		return domain.PaymentAttempt{}, fmt.Errorf("payments: stored attempt %x: %w", row.ID.Bytes, err)
	}
	return domain.RestoreAttempt(row.ID.Bytes, row.PaymentID.Bytes, domain.ProviderName(row.Provider), status,
		row.ExternalID.String, row.CreatedAt.Time.UTC(), row.UpdatedAt.Time.UTC()), nil
}
