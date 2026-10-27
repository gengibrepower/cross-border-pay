package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

func (p *Postgres) ClaimDueExpiries(ctx context.Context, now, leaseUntil time.Time, limit int) ([]domain.PaymentID, error) {
	rows, err := paymentsdb.New(p.pool).ClaimDueExpiries(ctx, paymentsdb.ClaimDueExpiriesParams{
		Now:        pgtype.Timestamptz{Time: now, Valid: true},
		LeaseUntil: pgtype.Timestamptz{Time: leaseUntil, Valid: true},
		MaxRows:    int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("payments: claim expiries: %w", err)
	}
	ids := make([]domain.PaymentID, len(rows))
	for i, id := range rows {
		ids[i] = id.Bytes
	}
	return ids, nil
}

// LatestAttempt returns the payment's newest attempt; ok is false when it has none.
func (p *Postgres) LatestAttempt(ctx context.Context, payment domain.PaymentID) (a domain.PaymentAttempt, ok bool, err error) {
	rows, err := paymentsdb.New(p.pool).LatestAttempts(ctx, []pgtype.UUID{{Bytes: payment, Valid: true}})
	if err != nil {
		return domain.PaymentAttempt{}, false, fmt.Errorf("payments: latest attempt of %s: %w", payment, err)
	}
	if len(rows) == 0 {
		return domain.PaymentAttempt{}, false, nil
	}
	a, err = restoreAttempt(&rows[0])
	if err != nil {
		return domain.PaymentAttempt{}, false, err
	}
	return a, true, nil
}

// Expire moves a pending payment whose quote has expired to expired and appends
// payment.expired, in one transaction. expired is false when there was nothing to do,
// such as a payment confirmed while the provider was being asked.
func (p *Postgres) Expire(ctx context.Context, id domain.PaymentID, now time.Time) (expired bool, err error) {
	err = postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		expired = false
		row, err := paymentsdb.New(tx).LockPayment(ctx, pgtype.UUID{Bytes: id, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("payment %s not found", id)
		}
		if err != nil {
			return fmt.Errorf("lock payment: %w", err)
		}
		if row.Status != string(domain.Pending) || now.Before(row.ExpiresAt.Time) {
			return nil
		}
		to, _, err := domain.Transition(domain.Pending, domain.Expire)
		if err != nil {
			return fmt.Errorf("payment: %w", err)
		}
		version, err := setStatus(ctx, tx, row.ID, to, now)
		if err != nil {
			return err
		}
		expired = true
		return notify(ctx, tx, p.outbox, domain.PaymentExpired, &storedPayment{
			id: row.ID, merchant: row.MerchantID, amount: row.Amount, currency: row.Currency,
		}, to, version, now)
	})
	if err != nil {
		return false, fmt.Errorf("payments: expire %s: %w", id, err)
	}
	return expired, nil
}
