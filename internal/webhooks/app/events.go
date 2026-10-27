package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	events "github.com/gengibrepower/cross-border-pay/internal/events/domain"
	merchantsapp "github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	merchants "github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

var (
	ErrInvalidLimit = fmt.Errorf("webhooks: limit must be between 1 and %d", MaxPageSize)
	// ErrEndpointMissing and ErrEndpointDisabled refuse a redelivery that could not
	// happen; the merchant fixes the endpoint with a PUT first.
	ErrEndpointMissing  = errors.New("webhooks: no webhook endpoint to redeliver to")
	ErrEndpointDisabled = errors.New("webhooks: webhook endpoint is disabled")
)

type EventStore interface {
	List(ctx context.Context, q *events.ListQuery) ([]events.Listed, error)
	// Redeliver returns events.ErrOutgoingNotFound when the merchant has no event at id.
	Redeliver(ctx context.Context, merchant, id [16]byte, now time.Time) (events.Listed, error)
}

type EndpointReader interface {
	Endpoint(ctx context.Context, merchant merchants.MerchantID) (merchants.Endpoint, error)
}

// Events is what a merchant sees of its outbox: each event with where its delivery
// stands, and the redelivery of any of them by hand.
type Events struct {
	store     EventStore
	endpoints EndpointReader
	clock     Clock
}

func NewEvents(store EventStore, endpoints EndpointReader, clock Clock) *Events {
	return &Events{store: store, endpoints: endpoints, clock: clock}
}

type Page struct {
	Events []events.Listed
	More   bool
}

func (s *Events) List(ctx context.Context, q events.ListQuery) (Page, error) {
	if q.Limit < 1 || q.Limit > MaxPageSize {
		return Page{}, ErrInvalidLimit
	}
	// One more than asked tells whether there is a next page.
	q.Limit++
	got, err := s.store.List(ctx, &q)
	if err != nil {
		return Page{}, fmt.Errorf("webhooks: list events: %w", err)
	}
	if len(got) == q.Limit {
		return Page{Events: got[:q.Limit-1], More: true}, nil
	}
	return Page{Events: got}, nil
}

// Redeliver makes the event due now in a new retry window (decision 23), whatever its
// status: a failed one is retried, a delivered one sent again for a merchant that lost
// it, a pending one sent without waiting for its backoff. It is idempotent by nature:
// repeating it only queues the event again.
func (s *Events) Redeliver(ctx context.Context, merchant merchants.MerchantID, id [16]byte) (events.Listed, error) {
	e, err := s.endpoints.Endpoint(ctx, merchant)
	switch {
	case errors.Is(err, merchantsapp.ErrEndpointNotFound):
		return events.Listed{}, ErrEndpointMissing
	case err != nil:
		return events.Listed{}, fmt.Errorf("webhooks: redeliver: %w", err)
	case e.Disabled():
		return events.Listed{}, ErrEndpointDisabled
	}
	l, err := s.store.Redeliver(ctx, merchant, id, s.clock.Now())
	if err != nil {
		return events.Listed{}, fmt.Errorf("webhooks: redeliver: %w", err)
	}
	return l, nil
}
