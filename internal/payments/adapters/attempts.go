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
	row, err := insertAttempt(ctx, paymentsdb.New(p.pool), payment, provider, now)
	if err != nil {
		return domain.PaymentAttempt{}, err
	}
	return restoreAttempt(&row)
}

func insertAttempt(ctx context.Context, q *paymentsdb.Queries, payment domain.PaymentID, provider domain.ProviderName, now time.Time) (paymentsdb.PaymentAttempt, error) {
	row, err := q.InsertAttempt(ctx, paymentsdb.InsertAttemptParams{
		PaymentID: pgtype.UUID{Bytes: payment, Valid: true},
		Provider:  string(provider),
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == oneLiveAttempt {
		return paymentsdb.PaymentAttempt{}, fmt.Errorf("%w: %s", app.ErrLiveAttempt, payment)
	}
	if err != nil {
		return paymentsdb.PaymentAttempt{}, fmt.Errorf("payments: insert attempt: %w", err)
	}
	return row, nil
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
// duplicate, which writes nothing. An attempt that becomes succeeded confirms its
// payment in the same transaction.
func (p *Postgres) ApplyAttempt(ctx context.Context, id domain.AttemptID, u domain.AttemptUpdate, now time.Time) (attempt domain.PaymentAttempt, changed bool, err error) {
	err = postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		attempt, changed, err = applyAttempt(ctx, tx, p.outbox, id, u, now)
		return err
	})
	if err != nil {
		return domain.PaymentAttempt{}, false, fmt.Errorf("payments: apply %s to attempt %s: %w", u.Event, id, err)
	}
	return attempt, changed, nil
}

func applyAttempt(ctx context.Context, tx pgx.Tx, outbox Outbox, id domain.AttemptID, u domain.AttemptUpdate, now time.Time) (domain.PaymentAttempt, bool, error) {
	pay, err := lockPayment(ctx, paymentsdb.New(tx), id)
	if err != nil {
		return domain.PaymentAttempt{}, false, err
	}
	return applyLocked(ctx, tx, outbox, &pay, id, u, now)
}

func lockPayment(ctx context.Context, q *paymentsdb.Queries, id domain.AttemptID) (paymentsdb.LockPaymentOfAttemptRow, error) {
	row, err := q.LockPaymentOfAttempt(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return paymentsdb.LockPaymentOfAttemptRow{}, app.ErrAttemptNotFound
	}
	if err != nil {
		return paymentsdb.LockPaymentOfAttemptRow{}, fmt.Errorf("lock payment: %w", err)
	}
	return row, nil
}

// applyLocked runs with pay already locked by lockPayment.
func applyLocked(ctx context.Context, tx pgx.Tx, outbox Outbox, pay *paymentsdb.LockPaymentOfAttemptRow, id domain.AttemptID, u domain.AttemptUpdate, now time.Time) (domain.PaymentAttempt, bool, error) {
	q := paymentsdb.New(tx)
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
	from := attempt.Status()
	changed, err := attempt.Apply(u, now)
	if err != nil {
		return domain.PaymentAttempt{}, false, fmt.Errorf("apply: %w", err)
	}
	if !changed {
		return attempt, false, nil
	}
	// A new status restarts the check schedule: sent and unknown enter the queue,
	// every other status leaves it.
	checks, next := row.Checks, row.NextCheckAt
	if attempt.Status() != from {
		at, ok := domain.FirstCheck(attempt.Status(), now)
		checks, next = 0, pgtype.Timestamptz{Time: at, Valid: ok}
	}
	err = q.UpdateAttempt(ctx, paymentsdb.UpdateAttemptParams{
		ID:          row.ID,
		Status:      string(attempt.Status()),
		ExternalID:  pgtype.Text{String: attempt.ExternalID(), Valid: attempt.ExternalID() != ""},
		BrCode:      pgtype.Text{String: attempt.BRCode(), Valid: attempt.BRCode() != ""},
		UpdatedAt:   pgtype.Timestamptz{Time: attempt.UpdatedAt(), Valid: true},
		Checks:      checks,
		NextCheckAt: next,
	})
	if err != nil {
		return domain.PaymentAttempt{}, false, fmt.Errorf("update: %w", err)
	}
	if attempt.Status() == domain.AttemptSucceeded {
		if err := confirm(ctx, tx, outbox, pay, u.PaidAt, now); err != nil {
			return domain.PaymentAttempt{}, false, fmt.Errorf("confirm: %w", err)
		}
	}
	return attempt, true, nil
}

func (p *Postgres) ClaimDueAttempts(ctx context.Context, now, leaseUntil time.Time, limit int) ([]app.DueAttempt, error) {
	rows, err := paymentsdb.New(p.pool).ClaimDueAttempts(ctx, paymentsdb.ClaimDueAttemptsParams{
		Now:        pgtype.Timestamptz{Time: now, Valid: true},
		LeaseUntil: pgtype.Timestamptz{Time: leaseUntil, Valid: true},
		MaxRows:    int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("payments: claim attempts: %w", err)
	}
	due := make([]app.DueAttempt, len(rows))
	for i := range rows {
		a, err := restoreAttempt(&rows[i])
		if err != nil {
			return nil, err
		}
		due[i] = app.DueAttempt{Attempt: a, Checks: int(rows[i].Checks)}
	}
	return due, nil
}

// RescheduleAttempt is a no-op once the attempt left unknown, such as when an event
// resolved it during the lookup.
func (p *Postgres) RescheduleAttempt(ctx context.Context, id domain.AttemptID, next time.Time) error {
	_, err := paymentsdb.New(p.pool).RescheduleAttempt(ctx, paymentsdb.RescheduleAttemptParams{
		ID:          pgtype.UUID{Bytes: id, Valid: true},
		NextCheckAt: pgtype.Timestamptz{Time: next, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("payments: reschedule attempt %s: %w", id, err)
	}
	return nil
}

func (p *Postgres) EscalateAttempt(ctx context.Context, id domain.AttemptID, now time.Time) error {
	_, err := paymentsdb.New(p.pool).EscalateAttempt(ctx, paymentsdb.EscalateAttemptParams{
		ID:          pgtype.UUID{Bytes: id, Valid: true},
		EscalatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("payments: escalate attempt %s: %w", id, err)
	}
	return nil
}

func restoreAttempt(row *paymentsdb.PaymentAttempt) (domain.PaymentAttempt, error) {
	status, err := domain.ParseAttemptStatus(row.Status)
	if err != nil {
		return domain.PaymentAttempt{}, fmt.Errorf("payments: stored attempt %x: %w", row.ID.Bytes, err)
	}
	return domain.RestoreAttempt(row.ID.Bytes, row.PaymentID.Bytes, domain.ProviderName(row.Provider), status,
		row.ExternalID.String, row.BrCode.String, row.CreatedAt.Time.UTC(), row.UpdatedAt.Time.UTC()), nil
}
