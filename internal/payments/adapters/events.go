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
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

// HandlePixSimEvent applies a simulator event, already verified, inside tx: the
// inbox's transaction, so the effect commits with the event's status. subject is the
// attempt it concerns. Money that confirms nothing, late or for another amount, is
// posted as a divergence and the event is settled. It returns app.ErrAttemptNotFound
// for an attempt not stored yet, app.ErrEventDivergent for an event that cannot be
// applied and app.ErrUnknownEventType.
func HandlePixSimEvent(ctx context.Context, tx pgx.Tx, sinks *Sinks, payload []byte, now time.Time) (subject string, err error) {
	var e pixsim.Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return "", fmt.Errorf("%w: %w", app.ErrEventDivergent, err)
	}
	if e.Type != pixsim.ChargePaid {
		return "", fmt.Errorf("%w: %q", app.ErrUnknownEventType, e.Type)
	}
	id, err := domain.ParseAttemptID(e.Charge.IdempotencyKey)
	if err != nil {
		return "", fmt.Errorf("%w: %w", app.ErrEventDivergent, err)
	}
	subject = id.String()
	if e.Charge.Status != pixsim.Paid {
		return subject, fmt.Errorf("%w: charge.paid with status %q", app.ErrEventDivergent, e.Charge.Status)
	}

	row, err := lockPayment(ctx, paymentsdb.New(tx), id)
	if err != nil {
		return subject, fmt.Errorf("payments: %s: %w", id, err)
	}

	u := domain.AttemptUpdate{Event: domain.Succeed, ExternalID: e.Charge.ID, BRCode: e.Charge.BRCode, Amount: e.Charge.Amount}
	if e.Charge.PaidAt != nil {
		u.PaidAt = e.Charge.PaidAt.UTC()
	}
	if err := applySaved(ctx, tx, sinks, &row, id, &u, now); errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrBRCodeMismatch) ||
		errors.Is(err, errUnpostable) {
		return subject, fmt.Errorf("%w: %w", app.ErrEventDivergent, err)
	} else if err != nil {
		return subject, fmt.Errorf("payments: apply event to %s: %w", id, err)
	}
	return subject, nil
}

// applySaved runs applyLocked in a savepoint. The inbox commits tx with a failed event's
// status, so whatever an event wrote before it turned out divergent must go first; the
// payment's lock, taken before the savepoint, is kept.
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
