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
	// CleanupBatch keeps each delete of the cleanup short; it loops until a batch
	// comes back short.
	CleanupBatch = 1000
)

var (
	errNoEndpoint       = errors.New("merchant has no webhook endpoint")
	errEndpointDisabled = errors.New("webhook endpoint disabled after only failures; PUT it again to turn it back on")
)

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
	// DeleteDelivered deletes up to limit events delivered at or before cutoff.
	DeleteDelivered(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

type Endpoints interface {
	// Endpoint returns merchantsapp.ErrEndpointNotFound for a merchant without one.
	Endpoint(ctx context.Context, merchant merchants.MerchantID) (merchants.Endpoint, error)
	// EndpointSucceeded and EndpointFailed feed decision 24's breaker. EndpointFailed
	// reports whether this failure disabled the endpoint.
	EndpointSucceeded(ctx context.Context, merchant merchants.MerchantID) error
	EndpointFailed(ctx context.Context, merchant merchants.MerchantID, reason string, now time.Time) (bool, error)
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
// runs out and they are due again, so every event is delivered at least once. An
// endpoint that is disabled gets no post: its events wait with the usual backoff.
func (d *Deliverer) Deliver(ctx context.Context, limit int) (int, error) {
	now := d.clock.Now()
	batch, err := d.queue.Claim(ctx, now, now.Add(d.timeout+LeaseMargin), PerMerchant, limit)
	if err != nil {
		return 0, fmt.Errorf("webhooks: %w", err)
	}
	answers := make([]domain.Answer, len(batch))
	posted := make([]bool, len(batch))
	endpoints := make(map[merchants.MerchantID]*merchants.Endpoint)
	var wg sync.WaitGroup
	for i := range batch {
		e, err := d.endpoint(ctx, endpoints, batch[i].Event.Merchant)
		switch {
		case err != nil:
			answers[i] = domain.Answer{Err: err}
		case e.Disabled():
			answers[i] = domain.Answer{Err: errEndpointDisabled}
		default:
			posted[i] = true
			wg.Go(func() { answers[i] = d.post(ctx, &batch[i], e) })
		}
	}
	wg.Wait()

	var errs []error
	for i := range batch {
		errs = append(errs, d.record(ctx, &batch[i], answers[i]))
	}
	errs = append(errs, d.trip(ctx, batch, answers, posted)...)
	return len(batch), errors.Join(errs...)
}

// trip feeds the round's posts to each merchant's breaker, once per merchant: a
// single 2xx means the endpoint works, even if other posts failed.
func (d *Deliverer) trip(ctx context.Context, batch []events.Delivery, answers []domain.Answer, posted []bool) []error {
	succeeded := make(map[merchants.MerchantID]bool)
	failure := make(map[merchants.MerchantID]string)
	var order []merchants.MerchantID
	for i := range batch {
		if !posted[i] {
			continue
		}
		m := merchants.MerchantID(batch[i].Event.Merchant)
		if _, seen := succeeded[m]; !seen {
			order = append(order, m)
		}
		succeeded[m] = succeeded[m] || answers[i].Succeeded()
		if !answers[i].Succeeded() {
			failure[m] = answers[i].Reason()
		}
	}
	var errs []error
	for _, m := range order {
		switch {
		case succeeded[m]:
			// Recorded even when stopping, like the 2xx itself: a run of failures left
			// open could disable an endpoint that works.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
			err := d.endpoints.EndpointSucceeded(rctx, m)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Errorf("webhooks: %w", err))
			}
		case ctx.Err() != nil:
			// Failures cut short by shutdown say nothing about the endpoint.
		default:
			disabled, err := d.endpoints.EndpointFailed(ctx, m, failure[m], d.clock.Now())
			if err != nil {
				errs = append(errs, fmt.Errorf("webhooks: %w", err))
			}
			if disabled {
				// The notice of decision 24: the merchant sees it in GET
				// /v1/webhook-endpoint, an operator here.
				d.logger.ErrorContext(ctx, "webhook endpoint disabled after only failures",
					"merchant", fmt.Sprintf("%x", m), "after", merchants.DisableAfter, "last_reason", failure[m])
			}
		}
	}
	return errs
}

// Cleanup deletes delivered events older than domain.DeliveredRetention, batch at a
// time. Failed and pending events are kept: they still need a delivery.
func (d *Deliverer) Cleanup(ctx context.Context, batch int) (int64, error) {
	cutoff := d.clock.Now().Add(-domain.DeliveredRetention)
	var total int64
	for {
		n, err := d.queue.DeleteDelivered(ctx, cutoff, batch)
		total += n
		if err != nil {
			return total, fmt.Errorf("webhooks: cleanup: %w", err)
		}
		if n < int64(batch) {
			return total, nil
		}
	}
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
