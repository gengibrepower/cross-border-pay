package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

// HandlePixSimEvent applies a simulator event, already verified, inside tx: the
// inbox's transaction, so the effect commits with the event's status. subject is the
// attempt it concerns. It returns app.ErrAttemptNotFound for an attempt not stored
// yet, app.ErrEventDivergent for one that contradicts it and app.ErrUnknownEventType.
func HandlePixSimEvent(ctx context.Context, tx pgx.Tx, payload []byte, now time.Time) (subject string, err error) {
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

	q := paymentsdb.New(tx)
	row, err := q.LockPaymentOfAttempt(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return subject, fmt.Errorf("%w: %s", app.ErrAttemptNotFound, id)
	}
	if err != nil {
		return subject, fmt.Errorf("payments: lock payment of %s: %w", id, err)
	}
	currency, err := money.ParseCurrency(row.QuoteCurrency)
	if err != nil {
		return subject, fmt.Errorf("payments: stored quote of %s: %w", id, err)
	}
	if quoted := money.New(row.PayerAmount, currency); e.Charge.Amount != quoted {
		return subject, fmt.Errorf("%w: paid %s, quoted %s", app.ErrEventDivergent, e.Charge.Amount, quoted)
	}

	u := domain.AttemptUpdate{Event: domain.Succeed, ExternalID: e.Charge.ID, BRCode: e.Charge.BRCode}
	_, _, err = applyAttempt(ctx, q, id, u, now)
	if errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrExternalIDMismatch) || errors.Is(err, domain.ErrBRCodeMismatch) {
		return subject, fmt.Errorf("%w: %w", app.ErrEventDivergent, err)
	}
	if err != nil {
		return subject, fmt.Errorf("payments: apply event to %s: %w", id, err)
	}
	return subject, nil
}
