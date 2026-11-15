package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/boletosim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

// HandlePixSimEvent runs in the inbox's transaction, so the effect commits with the
// event's status. Money that confirms nothing is posted as a divergence and the event
// is settled; app.ErrAttemptNotFound is an attempt not stored yet, retried.
func HandlePixSimEvent(ctx context.Context, tx pgx.Tx, sinks *Sinks, payload []byte, now time.Time) (subject string, err error) {
	var e pixsim.Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return "", fmt.Errorf("%w: %w", app.ErrEventDivergent, err)
	}
	if e.Type != pixsim.ChargePaid {
		return "", fmt.Errorf("%w: %q", app.ErrUnknownEventType, e.Type)
	}
	if e.Charge.Status != pixsim.Paid {
		return subjectOf(e.Charge.IdempotencyKey), fmt.Errorf("%w: charge.paid with status %q", app.ErrEventDivergent, e.Charge.Status)
	}
	u := domain.AttemptUpdate{Event: domain.Succeed, ExternalID: e.Charge.ID, BRCode: e.Charge.BRCode, Amount: e.Charge.Amount}
	if e.Charge.PaidAt != nil {
		u.PaidAt = e.Charge.PaidAt.UTC()
	}
	return applyPaid(ctx, tx, sinks, PixSimName, e.Charge.IdempotencyKey, &u, now)
}

// HandleBoletoSimEvent is HandlePixSimEvent for the boleto simulator's boleto.paid.
func HandleBoletoSimEvent(ctx context.Context, tx pgx.Tx, sinks *Sinks, payload []byte, now time.Time) (subject string, err error) {
	var e boletosim.Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return "", fmt.Errorf("%w: %w", app.ErrEventDivergent, err)
	}
	if e.Type != boletosim.BoletoPaid {
		return "", fmt.Errorf("%w: %q", app.ErrUnknownEventType, e.Type)
	}
	if e.Boleto.Status != boletosim.Paid {
		return subjectOf(e.Boleto.IdempotencyKey), fmt.Errorf("%w: boleto.paid with status %q", app.ErrEventDivergent, e.Boleto.Status)
	}
	u := domain.AttemptUpdate{Event: domain.Succeed, ExternalID: e.Boleto.ID, Barcode: e.Boleto.Barcode, Amount: e.Boleto.Amount}
	if e.Boleto.PaidAt != nil {
		u.PaidAt = e.Boleto.PaidAt.UTC()
	}
	return applyPaid(ctx, tx, sinks, BoletoSimName, e.Boleto.IdempotencyKey, &u, now)
}

func subjectOf(key string) string {
	id, err := domain.ParseAttemptID(key)
	if err != nil {
		return ""
	}
	return id.String()
}

// applyPaid: an attempt another provider holds is not this provider's to confirm,
// whatever ID its event names.
func applyPaid(ctx context.Context, tx pgx.Tx, sinks *Sinks, provider domain.ProviderName, key string, u *domain.AttemptUpdate, now time.Time) (subject string, err error) {
	id, err := domain.ParseAttemptID(key)
	if err != nil {
		return "", fmt.Errorf("%w: %w", app.ErrEventDivergent, err)
	}
	subject = id.String()
	row, err := lockPayment(ctx, paymentsdb.New(tx), id)
	if err != nil {
		return subject, fmt.Errorf("payments: %s: %w", id, err)
	}
	if row.AttemptProvider != string(provider) {
		return subject, fmt.Errorf("%w: attempt %s is held by %s, not %s", app.ErrEventDivergent, id, row.AttemptProvider, provider)
	}
	if err := applySaved(ctx, tx, sinks, &row, id, u, now); errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrBRCodeMismatch) ||
		errors.Is(err, domain.ErrBarcodeMismatch) || errors.Is(err, errUnpostable) {
		return subject, fmt.Errorf("%w: %w", app.ErrEventDivergent, err)
	} else if err != nil {
		return subject, fmt.Errorf("payments: apply event to %s: %w", id, err)
	}
	return subject, nil
}

// applySaved uses a savepoint because the inbox commits tx with a failed event's
// status: what a divergent event wrote must go, and the payment's lock, taken before,
// stays.
func applySaved(ctx context.Context, tx pgx.Tx, sinks *Sinks, pay *paymentsdb.LockPaymentOfAttemptRow, id domain.AttemptID, u *domain.AttemptUpdate, now time.Time) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	if _, _, err := applyLocked(ctx, sp, sinks, pay, id, u, now); err != nil {
		if rerr := sp.Rollback(ctx); rerr != nil {
			return errors.Join(err, fmt.Errorf("rollback to savepoint: %w", rerr))
		}
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}
