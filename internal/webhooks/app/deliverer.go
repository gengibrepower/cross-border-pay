// Package app runs the worker that delivers the outbox to merchants' endpoints: each
// round claims due events under a lease, posts them outside any transaction and
// records how each attempt ended.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	events "github.com/gengibrepower/cross-border-pay/internal/events/domain"
	merchantsapp "github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	merchants "github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/webhooks/domain"
)

const (
	// DeliverBatch bounds one round; PerMerchant is how many of a round's posts one
	// merchant may have, so a slow or failing merchant cannot take the whole round.
	DeliverBatch = 32
	PerMerchant  = 4
	// LeaseMargin is how long a claim outlives its post's timeout, to record the
	// answers. A worker that dies keeps its events away for timeout + LeaseMargin.
	LeaseMargin = time.Minute
	// recordTimeout bounds recording a 2xx after the worker was told to stop.
	recordTimeout = 5 * time.Second
)

var errNoEndpoint = errors.New("merchant has no webhook endpoint")

type Clock interface {
	Now() time.Time
}

type Queue interface {
	Claim(ctx context.Context, now, leaseUntil time.Time, perMerchant, maxEvents int) ([]events.Delivery, error)
	// Delivered, Reschedule and Fail report false when the event was not changed:
	// already finished, or claimed again after this claim's lease ran out.
	Delivered(ctx context.Context, d *events.Delivery, statusCode int, now time.Time) (bool, error)
	Reschedule(ctx context.Context, d *events.Delivery, statusCode int, reason string, at time.Time) (bool, error)
	Fail(ctx context.Context, d *events.Delivery, statusCode int, reason string, now time.Time) (bool, error)
}

type Endpoints interface {
	// Endpoint returns merchantsapp.ErrEndpointNotFound for a merchant without one.
	Endpoint(ctx context.Context, merchant merchants.MerchantID) (merchants.Endpoint, error)
}

type Sender interface {
	// Send posts body with the signature header. An answer with any status is not an
	// error; no answer is.
	Send(ctx context.Context, url, signature string, body []byte) (statusCode int, err error)
}

// Jitter draws a wait from [0, ceiling].
type Jitter func(ceiling time.Duration) time.Duration

func FullJitter(ceiling time.Duration) time.Duration {
	return rand.N(ceiling + 1) //nolint:gosec // G404: spreads retries over time; it guards nothing.
}

type Deliverer struct {
	queue     Queue
	endpoints Endpoints
	sender    Sender
	key       []byte
	clock     Clock
	jitter    Jitter
	timeout   time.Duration
	logger    *slog.Logger
}

// NewDeliverer takes the key the merchants' secrets are derived from and the timeout
// of each post.
func NewDeliverer(queue Queue, endpoints Endpoints, sender Sender, signingKey []byte, clock Clock, jitter Jitter, timeout time.Duration, logger *slog.Logger) (*Deliverer, error) {
	if len(signingKey) < merchants.MinSigningKeyBytes {
		return nil, merchants.ErrWeakSigningKey
	}
	return &Deliverer{
		queue: queue, endpoints: endpoints, sender: sender, key: signingKey,
		clock: clock, jitter: jitter, timeout: timeout, logger: logger,
	}, nil
}

// Deliver runs one round over up to limit due events and returns how many it claimed.
// The posts run in parallel and the round waits for all of them, each bounded by the
// timeout. When ctx ends mid-round, interrupted posts are not recorded: their lease
// runs out and they are due again, so every event is delivered at least once.
func (d *Deliverer) Deliver(ctx context.Context, limit int) (int, error) {
	now := d.clock.Now()
	batch, err := d.queue.Claim(ctx, now, now.Add(d.timeout+LeaseMargin), PerMerchant, limit)
	if err != nil {
		return 0, fmt.Errorf("webhooks: %w", err)
	}
	answers := make([]domain.Answer, len(batch))
	endpoints := make(map[merchants.MerchantID]*merchants.Endpoint)
	var wg sync.WaitGroup
	for i := range batch {
		e, err := d.endpoint(ctx, endpoints, batch[i].Event.Merchant)
		if err != nil {
			answers[i] = domain.Answer{Err: err}
			continue
		}
		wg.Go(func() { answers[i] = d.post(ctx, &batch[i], e) })
	}
	wg.Wait()

	var errs []error
	for i := range batch {
		errs = append(errs, d.record(ctx, &batch[i], answers[i]))
	}
	return len(batch), errors.Join(errs...)
}

func (d *Deliverer) endpoint(ctx context.Context, cache map[merchants.MerchantID]*merchants.Endpoint, merchant [16]byte) (*merchants.Endpoint, error) {
	if e, ok := cache[merchant]; ok {
		return e, nil
	}
	e, err := d.endpoints.Endpoint(ctx, merchant)
	if errors.Is(err, merchantsapp.ErrEndpointNotFound) {
		return nil, errNoEndpoint
	}
	if err != nil {
		return nil, fmt.Errorf("webhooks: %w", err)
	}
	cache[merchant] = &e
	return &e, nil
}

func (d *Deliverer) post(ctx context.Context, dl *events.Delivery, e *merchants.Endpoint) domain.Answer {
	now := d.clock.Now()
	versions := domain.SigningVersions(e, now)
	secrets := make([]string, len(versions))
	for i, v := range versions {
		s, err := merchants.DeriveSecret(d.key, e.Merchant, v)
		if err != nil {
			return domain.Answer{Err: fmt.Errorf("webhooks: secret: %w", err)}
		}
		secrets[i] = s
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	code, err := d.sender.Send(ctx, e.URL, domain.Sign(secrets, now, dl.Event.Payload), dl.Event.Payload)
	if err != nil {
		return domain.Answer{Err: err}
	}
	return domain.Answer{StatusCode: code}
}

func (d *Deliverer) record(ctx context.Context, dl *events.Delivery, a domain.Answer) error {
	now := d.clock.Now()
	attrs := []any{"event", fmt.Sprintf("%x", dl.Event.ID), "type", dl.Event.Type, "subject", dl.Event.Subject, "attempt", dl.Attempt}
	var changed bool
	var err error
	switch next := domain.Decide(a, dl.Event.CreatedAt, now); next {
	case domain.Delivered:
		// The merchant has the event: record it even when the worker is stopping, or
		// it would be sent again for nothing.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		defer cancel()
		if changed, err = d.queue.Delivered(rctx, dl, a.StatusCode, now); changed {
			d.logger.InfoContext(ctx, "webhook delivered", append(attrs, "status", a.StatusCode)...)
		}
	case domain.Retry:
		if ctx.Err() != nil {
			// Cut short by shutdown, not refused by the merchant: the lease brings it back.
			return nil
		}
		at := now.Add(d.jitter(domain.RetryCeiling(dl.Attempt)))
		if changed, err = d.queue.Reschedule(ctx, dl, a.StatusCode, a.Reason(), at); changed {
			d.logger.WarnContext(ctx, "webhook failed, will retry", append(attrs, "reason", a.Reason(), "retry_at", at)...)
		}
	case domain.GiveUp:
		if ctx.Err() != nil {
			return nil
		}
		if changed, err = d.queue.Fail(ctx, dl, a.StatusCode, a.Reason(), now); changed {
			d.logger.ErrorContext(ctx, "webhook given up, redeliver by hand", append(attrs, "reason", a.Reason())...)
		}
	default:
		return fmt.Errorf("webhooks: unknown next step %d", next)
	}
	if err != nil {
		return fmt.Errorf("webhooks: record: %w", err)
	}
	if !changed {
		d.logger.InfoContext(ctx, "webhook attempt superseded by a later claim", attrs...)
	}
	return nil
}
