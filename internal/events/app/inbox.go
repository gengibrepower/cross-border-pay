// Package app receives provider events into the inbox and runs the worker that applies
// them. It knows no provider and no module: verifiers and handlers are plugged in by
// cmd/api and cmd/worker.
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gengibrepower/cross-border-pay/internal/events/domain"
)

var (
	ErrUnknownProvider = errors.New("events: unknown provider")
	// ErrUnauthenticated is a signature that does not prove the provider sent the event.
	ErrUnauthenticated = errors.New("events: event not authenticated")
	// ErrMalformedEvent is an authentic body the gateway cannot read.
	ErrMalformedEvent = errors.New("events: malformed event")
)

type Clock interface {
	Now() time.Time
}

// Verifier checks one provider's signature and names the event. header reads a request
// header; it returns ErrUnauthenticated or ErrMalformedEvent, wrapped.
type Verifier interface {
	Verify(header func(string) string, body []byte, now time.Time) (domain.Incoming, error)
}

type Store interface {
	// Insert stores the event once per provider and external ID. For a duplicate,
	// inserted is false and stored is the payload kept from the first delivery.
	Insert(ctx context.Context, in *domain.Incoming, now time.Time) (inserted bool, stored []byte, err error)
}

type Inbox struct {
	store     Store
	verifiers map[string]Verifier
	clock     Clock
	logger    *slog.Logger
}

func NewInbox(store Store, verifiers map[string]Verifier, clock Clock, logger *slog.Logger) *Inbox {
	return &Inbox{store: store, verifiers: verifiers, clock: clock, logger: logger}
}

// Receive verifies and stores the event; a duplicate is not an error, so the provider
// stops redelivering it.
func (i *Inbox) Receive(ctx context.Context, provider string, header func(string) string, body []byte) error {
	v, ok := i.verifiers[provider]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
	now := i.clock.Now()
	in, err := v.Verify(header, body, now)
	if err != nil {
		return fmt.Errorf("events: %s: %w", provider, err)
	}
	in.Provider = provider
	if err := in.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedEvent, err)
	}
	inserted, stored, err := i.store.Insert(ctx, &in, now)
	if err != nil {
		return fmt.Errorf("events: receive: %w", err)
	}
	if !inserted && !bytes.Equal(stored, in.Payload) {
		// The provider signed two different bodies with one event ID. The first one
		// stays; the second is kept only in the log, for a person to compare.
		i.logger.ErrorContext(ctx, "event redelivered with a different payload",
			"provider", provider, "external_id", in.ExternalID, "payload", string(in.Payload))
	}
	return nil
}

// Result is how a handler ended an event. Subject names what it changed, such as an
// attempt; Reason explains a Retry or a Failed.
type Result struct {
	Outcome domain.Outcome
	Subject string
	Reason  string
}

// Handler applies an event inside tx, the transaction that also records its new
// status, so the two commit together or not at all.
type Handler interface {
	Handle(ctx context.Context, tx pgx.Tx, e *domain.Event) (Result, error)
}

// Done is an event the queue finished one attempt at.
type Done struct {
	Event  domain.Event
	Result Result
	Status domain.Status
}

type Queue interface {
	// ProcessNext claims the oldest due event, runs handle on it and records the
	// result, all in one transaction. found is false when nothing is due. If handle
	// fails, nothing is recorded and Done carries the event for Retry.
	ProcessNext(ctx context.Context, now time.Time, handle func(context.Context, pgx.Tx, *domain.Event) (Result, error)) (d Done, found bool, err error)
	// Retry records a failed attempt outside the rolled-back transaction.
	Retry(ctx context.Context, e *domain.Event, reason string, now time.Time) (domain.Status, error)
}

// ProcessBatch bounds one run, so the job's interval paces a backlog.
const ProcessBatch = 10

type Processor struct {
	queue   Queue
	handler Handler
	clock   Clock
	logger  *slog.Logger
}

func NewProcessor(queue Queue, handler Handler, clock Clock, logger *slog.Logger) *Processor {
	return &Processor{queue: queue, handler: handler, clock: clock, logger: logger}
}

// Process handles up to limit due events, each in its own transaction, so one bad
// event does not hold back the others. It returns how many it took.
func (p *Processor) Process(ctx context.Context, limit int) (int, error) {
	var errs []error
	n := 0
	for ; n < limit; n++ {
		d, found, err := p.queue.ProcessNext(ctx, p.clock.Now(), p.handler.Handle)
		if !found {
			return n, errors.Join(append(errs, err)...)
		}
		if err != nil {
			// The job logs the joined errors; Retry only reschedules.
			_, rerr := p.queue.Retry(ctx, &d.Event, err.Error(), p.clock.Now())
			errs = append(errs, err, rerr)
			continue
		}
		p.log(ctx, &d)
	}
	return n, errors.Join(errs...)
}

func (p *Processor) log(ctx context.Context, d *Done) {
	attrs := []any{"provider", d.Event.Provider, "event", d.Event.ExternalID, "type", d.Event.Type, "subject", d.Result.Subject}
	switch d.Status {
	case domain.StatusFailed:
		p.logger.ErrorContext(ctx, "event failed, needs review", append(attrs, "reason", d.Result.Reason)...)
	case domain.StatusPending:
		p.logger.WarnContext(ctx, "event not applicable yet", append(attrs, "reason", d.Result.Reason, "attempts", d.Event.Attempts+1)...)
	case domain.StatusIgnored:
		p.logger.InfoContext(ctx, "event ignored", attrs...)
	case domain.StatusProcessed:
		p.logger.InfoContext(ctx, "event processed", attrs...)
	default:
		p.logger.ErrorContext(ctx, "event left in an unknown status", append(attrs, "status", d.Status)...)
	}
}
