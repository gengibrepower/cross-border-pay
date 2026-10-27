package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsadapters "github.com/gengibrepower/cross-border-pay/internal/events/adapters"
	eventsapp "github.com/gengibrepower/cross-border-pay/internal/events/app"
	events "github.com/gengibrepower/cross-border-pay/internal/events/domain"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// paymentEvents is the bridge that keeps events and payments apart: it hands each
// provider's events to payments and reads payments' errors as inbox outcomes.
type paymentEvents struct {
	clock  eventsapp.Clock
	outbox paymentsadapters.Outbox
}

func (h paymentEvents) Handle(ctx context.Context, tx pgx.Tx, e *events.Event) (eventsapp.Result, error) {
	if e.Provider != string(paymentsadapters.PixSimName) {
		return eventsapp.Result{Outcome: events.Ignored, Reason: "no handler for provider " + e.Provider}, nil
	}
	subject, err := paymentsadapters.HandlePixSimEvent(ctx, tx, h.outbox, e.Payload, h.clock.Now())
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

// merchantOutbox is the bridge that keeps payments from importing events: it hands
// payments' events to the events outbox, in payments' transaction.
type merchantOutbox struct {
	outbox eventsadapters.Outbox
}

func (o merchantOutbox) Append(ctx context.Context, tx pgx.Tx, e *payments.MerchantEvent) error {
	err := o.outbox.Append(ctx, tx, &events.Outgoing{
		ID: e.ID, Merchant: e.Merchant, Type: string(e.Type), Subject: e.Payment.String(),
		Payload: e.Payload, CreatedAt: e.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("merchant outbox: %w", err)
	}
	return nil
}
