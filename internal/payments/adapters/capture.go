package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	fxadapters "github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

// PrepareCapture holds the payment's lock, the one a confirmation takes, while it
// decides: so a capture and the event or lookup that confirms it never cross, and the
// captured quote is written once.
func (p *Postgres) PrepareCapture(ctx context.Context, merchant domain.MerchantID, id domain.PaymentID, amount *money.Money, now time.Time) (app.PreparedCapture, error) {
	var c app.PreparedCapture
	err := postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = prepareCapture(ctx, tx, merchant, id, amount, now)
		return err
	})
	if err != nil {
		return app.PreparedCapture{}, fmt.Errorf("payments: prepare capture: %w", err)
	}
	return c, nil
}

func prepareCapture(ctx context.Context, tx pgx.Tx, merchant domain.MerchantID, id domain.PaymentID, amount *money.Money, now time.Time) (app.PreparedCapture, error) {
	q := paymentsdb.New(tx)
	pay, err := q.LockCapturablePayment(ctx, paymentsdb.LockCapturablePaymentParams{
		MerchantID: pgtype.UUID{Bytes: merchant, Valid: true},
		ID:         pgtype.UUID{Bytes: id, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return app.PreparedCapture{}, app.ErrPaymentNotFound
	}
	if err != nil {
		return app.PreparedCapture{}, fmt.Errorf("lock payment: %w", err)
	}
	if domain.Method(pay.Method) != domain.Card {
		return app.PreparedCapture{}, fmt.Errorf("%w: a %s payment has no authorization", app.ErrNotCapturable, pay.Method)
	}
	status, err := domain.ParseStatus(pay.Status)
	if err != nil {
		return app.PreparedCapture{}, fmt.Errorf("stored payment: %w", err)
	}
	quote, err := loadQuote(ctx, tx, pay.QuoteID.Bytes)
	if err != nil {
		return app.PreparedCapture{}, err
	}
	var captured *fx.Quote
	if pay.CapturedQuoteID.Valid {
		cq, err := loadQuote(ctx, tx, pay.CapturedQuoteID.Bytes)
		if err != nil {
			return app.PreparedCapture{}, err
		}
		captured = &cq
	}

	switch status {
	case domain.Pending:
	case domain.Paid, domain.PartiallyRefunded, domain.Refunded, domain.ChargedBack:
		// Paid already: a retry of the same capture, or of a card captured at once,
		// is answered with the payment as it is.
		took := &quote
		if captured != nil {
			took = captured
		}
		if amount != nil && *amount != took.Amount() {
			return app.PreparedCapture{}, fmt.Errorf("%w: %s was captured", app.ErrCaptureConflict, took.Amount())
		}
		return app.PreparedCapture{Done: true}, nil
	case domain.Expired, domain.Canceled:
		return app.PreparedCapture{}, fmt.Errorf("%w: payment is %s", app.ErrNotCapturable, status)
	default:
		return app.PreparedCapture{}, fmt.Errorf("%w: payment is %s", app.ErrNotCapturable, status)
	}

	attempt, err := latestAttempt(ctx, q, pay.ID)
	if err != nil {
		return app.PreparedCapture{}, err
	}
	switch attempt.Status() {
	case domain.AttemptAccepted:
	case domain.AttemptUnknown, domain.AttemptSent, domain.AttemptCreated:
		return app.PreparedCapture{}, fmt.Errorf("%w: attempt is %s", app.ErrCaptureInProgress, attempt.Status())
	case domain.AttemptRejected, domain.AttemptSucceeded:
		return app.PreparedCapture{}, fmt.Errorf("%w: attempt is %s", app.ErrNotCapturable, attempt.Status())
	default:
		return app.PreparedCapture{}, fmt.Errorf("%w: attempt is %s", app.ErrNotCapturable, attempt.Status())
	}
	if quote.Expired(now) {
		return app.PreparedCapture{}, fmt.Errorf("%w: at %s", app.ErrAuthorizationExpired, quote.ExpiresAt().UTC().Format(time.RFC3339))
	}

	if captured != nil {
		// An earlier capture got this far: the same one is tried again, never another,
		// because the acquirer may still apply the first.
		if amount != nil && *amount != captured.Amount() {
			return app.PreparedCapture{}, fmt.Errorf("%w: a capture of %s was started", app.ErrCaptureConflict, captured.Amount())
		}
		return app.PreparedCapture{Attempt: attempt, Quote: *captured}, nil
	}
	want := quote.Amount()
	if amount != nil {
		want = *amount
	}
	took, err := domain.PriceCapture(&quote, want)
	if err != nil {
		return app.PreparedCapture{}, fmt.Errorf("price: %w", err)
	}
	tookID := pay.QuoteID.Bytes
	if took.Amount() != quote.Amount() {
		if tookID, err = fxadapters.SaveQuote(ctx, tx, &took); err != nil {
			return app.PreparedCapture{}, fmt.Errorf("captured quote: %w", err)
		}
	}
	if err := q.SetCapturedQuote(ctx, paymentsdb.SetCapturedQuoteParams{
		ID: pay.ID, CapturedQuoteID: pgtype.UUID{Bytes: tookID, Valid: true},
	}); err != nil {
		return app.PreparedCapture{}, fmt.Errorf("set captured quote: %w", err)
	}
	return app.PreparedCapture{Attempt: attempt, Quote: took}, nil
}

func latestAttempt(ctx context.Context, q *paymentsdb.Queries, payment pgtype.UUID) (domain.PaymentAttempt, error) {
	rows, err := q.LatestAttempts(ctx, []pgtype.UUID{payment})
	if err != nil {
		return domain.PaymentAttempt{}, fmt.Errorf("latest attempt: %w", err)
	}
	if len(rows) == 0 {
		return domain.PaymentAttempt{}, fmt.Errorf("%w: payment has no attempt", app.ErrNotCapturable)
	}
	return restoreAttempt(&rows[0])
}
