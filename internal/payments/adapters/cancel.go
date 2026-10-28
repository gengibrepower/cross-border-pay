package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

// Cancel moves a pending payment to canceled and appends payment.canceled, in one
// transaction. The status is read under the payment's lock, so a confirmation that
// commits first makes the cancel fail instead of being overwritten.
func (p *Postgres) Cancel(ctx context.Context, merchant domain.MerchantID, id domain.PaymentID, now time.Time) error {
	err := postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		row, err := paymentsdb.New(tx).LockMerchantPayment(ctx, paymentsdb.LockMerchantPaymentParams{
			MerchantID: pgtype.UUID{Bytes: merchant, Valid: true},
			ID:         pgtype.UUID{Bytes: id, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrPaymentNotFound
		}
		if err != nil {
			return fmt.Errorf("lock payment: %w", err)
		}
		from, err := domain.ParseStatus(row.Status)
		if err != nil {
			return fmt.Errorf("stored payment: %w", err)
		}
		to, changed, err := domain.Transition(from, domain.Cancel)
		if err != nil {
			return fmt.Errorf("payment: %w", err)
		}
		if !changed {
			return nil
		}
		version, err := setStatus(ctx, tx, row.ID, to, now)
		if err != nil {
			return err
		}
		return notify(ctx, tx, p.sinks.Outbox, domain.PaymentCanceled, &storedPayment{
			id: row.ID, merchant: row.MerchantID, amount: row.Amount, currency: row.Currency,
		}, to, version, now)
	})
	if err != nil {
		return fmt.Errorf("payments: cancel %s: %w", id, err)
	}
	return nil
}
