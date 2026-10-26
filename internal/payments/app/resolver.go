package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

const (
	// ResolveBatch attempts are looked up one after another per round; at the provider
	// timeout of 5 s that is 50 s at worst, inside ClaimLease.
	ResolveBatch = 10
	ClaimLease   = time.Minute
)

// DueAttempt is a claimed attempt with the number of lookups that already failed.
type DueAttempt struct {
	Attempt domain.PaymentAttempt
	Checks  int
}

type AttemptQueue interface {
	// ClaimDueAttempts takes up to limit attempts whose check is due at now and moves
	// their next check to leaseUntil, so no other worker takes them meanwhile.
	ClaimDueAttempts(ctx context.Context, now, leaseUntil time.Time, limit int) ([]DueAttempt, error)
	ApplyAttempt(ctx context.Context, id domain.AttemptID, u domain.AttemptUpdate, now time.Time) (domain.PaymentAttempt, bool, error)
	// RescheduleAttempt records one more failed lookup of an unknown attempt.
	RescheduleAttempt(ctx context.Context, id domain.AttemptID, next time.Time) error
	// EscalateAttempt takes the attempt out of the queue for a person to resolve.
	EscalateAttempt(ctx context.Context, id domain.AttemptID, now time.Time) error
}

// Resolver finds out what happened to ambiguous attempts by asking the provider
// (decision 17): never by repeating the call.
type Resolver struct {
	queue           AttemptQueue
	provider        Provider
	clock           Clock
	logger          *slog.Logger
	providerTimeout time.Duration
}

func NewResolver(queue AttemptQueue, provider Provider, clock Clock, logger *slog.Logger, providerTimeout time.Duration) *Resolver {
	return &Resolver{queue: queue, provider: provider, clock: clock, logger: logger, providerTimeout: providerTimeout}
}

// Resolve checks one batch of due attempts and returns how many it claimed. One
// attempt failing does not stop the others; their errors are joined.
func (r *Resolver) Resolve(ctx context.Context, limit int) (int, error) {
	now := r.clock.Now()
	due, err := r.queue.ClaimDueAttempts(ctx, now, now.Add(ClaimLease), limit)
	if err != nil {
		return 0, fmt.Errorf("payments: claim attempts: %w", err)
	}
	var errs []error
	for i := range due {
		if ctx.Err() != nil {
			break
		}
		if err := r.check(ctx, &due[i]); err != nil {
			errs = append(errs, fmt.Errorf("payments: check attempt %s: %w", due[i].Attempt.ID(), err))
		}
	}
	return len(due), errors.Join(errs...)
}

func (r *Resolver) check(ctx context.Context, d *DueAttempt) error {
	a := &d.Attempt
	now := r.clock.Now()
	if now.Sub(a.CreatedAt()) >= domain.ReviewAfter {
		r.logger.ErrorContext(ctx, "attempt still ambiguous after the review limit; a person must resolve it",
			"attempt", a.ID().String(), "payment", a.Payment().String(), "status", string(a.Status()), "checks", d.Checks)
		if err := r.queue.EscalateAttempt(ctx, a.ID(), now); err != nil {
			return fmt.Errorf("escalate: %w", err)
		}
		return nil
	}

	callCtx, cancel := context.WithTimeout(ctx, r.providerTimeout)
	result, err := r.provider.GetAttempt(callCtx, a.ID())
	cancel()
	now = r.clock.Now()
	switch {
	case err == nil:
		u := domain.AttemptUpdate{Event: result.Event(), ExternalID: result.ExternalID}
		if result.Pix != nil {
			u.BRCode = result.Pix.BRCode
		}
		return r.apply(ctx, a, u, now)
	case errors.Is(err, ErrNotAtProvider) && now.Sub(a.CreatedAt()) >= domain.NotFoundGrace:
		// Our create gave up long ago, so nothing can still reach the provider: the
		// attempt never existed there.
		return r.apply(ctx, a, domain.AttemptUpdate{Event: domain.Reject}, now)
	default:
		r.logger.WarnContext(ctx, "attempt lookup did not resolve it", "attempt", a.ID().String(), "checks", d.Checks+1, "error", err)
		if a.Status() == domain.AttemptSent {
			// A stale sent becomes unknown, whose first check is scheduled with it.
			return r.apply(ctx, a, domain.AttemptUpdate{Event: domain.Ambiguous}, now)
		}
		if err := r.queue.RescheduleAttempt(ctx, a.ID(), now.Add(domain.CheckBackoff(d.Checks+1))); err != nil {
			return fmt.Errorf("reschedule: %w", err)
		}
		return nil
	}
}

func (r *Resolver) apply(ctx context.Context, a *domain.PaymentAttempt, u domain.AttemptUpdate, now time.Time) error {
	if _, _, err := r.queue.ApplyAttempt(ctx, a.ID(), u, now); err != nil {
		return fmt.Errorf("apply %s: %w", u.Event, err)
	}
	return nil
}
