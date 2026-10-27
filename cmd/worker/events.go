package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsapp "github.com/gengibrepower/cross-border-pay/internal/events/app"
	events "github.com/gengibrepower/cross-border-pay/internal/events/domain"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
)

// paymentEvents is the bridge that keeps events and payments apart: it hands each
// provider's events to payments and reads payments' errors as inbox outcomes.
type paymentEvents struct {
	clock eventsapp.Clock
}

func (h paymentEvents) Handle(ctx context.Context, tx pgx.Tx, e *events.Event) (eventsapp.Result, error) {
	if e.Provider != string(paymentsadapters.PixSimName) {
		return eventsapp.Result{Outcome: events.Ignored, Reason: "no handler for provider " + e.Provider}, nil
	}
	subject, err := paymentsadapters.HandlePixSimEvent(ctx, tx, e.Payload, h.clock.Now())
	r := eventsapp.Result{Subject: subject}
	switch {
	case err == nil:
		r.Outcome = events.Processed
	case errors.Is(err, paymentsapp.ErrUnknownEventType):
		r.Outcome, r.Reason = events.Ignored, err.Error()
	case errors.Is(err, paymentsapp.ErrAttemptNotFound):
		r.Outcome, r.Reason = events.Retry, err.Error()
	case errors.Is(err, paymentsapp.ErrEventDivergent):
		r.Outcome, r.Reason = events.Failed, err.Error()
	default:
		return eventsapp.Result{}, fmt.Errorf("payments: %w", err)
	}
	return r, nil
}
