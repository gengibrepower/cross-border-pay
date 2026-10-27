package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// ExpiryBatch payments are checked one after another per round, each with at most one
// provider lookup, so a round fits inside ClaimLease.
const ExpiryBatch = 10

type ExpiryQueue interface {
	// ClaimDueExpiries takes up to limit pending payments whose quote expired by now and
	// moves their next look to leaseUntil, so no other worker takes them meanwhile.
	ClaimDueExpiries(ctx context.Context, now, leaseUntil time.Time, limit int) ([]domain.PaymentID, error)
	LatestAttempt(ctx context.Context, payment domain.PaymentID) (domain.PaymentAttempt, bool, error)
	ApplyAttempt(ctx context.Context, id domain.AttemptID, u *domain.AttemptUpdate, now time.Time) (domain.PaymentAttempt, bool, error)
	// Expire returns false when the payment left pending meanwhile.
	Expire(ctx context.Context, id domain.PaymentID, now time.Time) (bool, error)
}

// Expirer closes payments whose quote ran out. It expires one only after asking the
// provider: a payer who paid on time is confirmed, however late the news of it is, and
// money paid late is posted as a divergence before the payment expires.
type Expirer struct {
	queue           ExpiryQueue
	provider        Provider
	clock           Clock
	logger          *slog.Logger
	providerTimeout time.Duration
}

func NewExpirer(queue ExpiryQueue, provider Provider, clock Clock, logger *slog.Logger, providerTimeout time.Duration) *Expirer {
	return &Expirer{queue: queue, provider: provider, clock: clock, logger: logger, providerTimeout: providerTimeout}
}

// Expire checks one batch of due payments and returns how many it claimed. One payment
// failing does not stop the others; their errors are joined.
func (x *Expirer) Expire(ctx context.Context, limit int) (int, error) {
	now := x.clock.Now()
	due, err := x.queue.ClaimDueExpiries(ctx, now, now.Add(ClaimLease), limit)
	if err != nil {
		return 0, fmt.Errorf("payments: claim expiries: %w", err)
	}
	var errs []error
	for _, id := range due {
		if ctx.Err() != nil {
			break
		}
		if err := x.check(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("payments: expire %s: %w", id, err))
		}
	}
	return len(due), errors.Join(errs...)
}

// check decides nothing when the provider cannot answer: the payment stays pending and
// comes back once the lease ends (decision 17's rule, applied to expiry).
func (x *Expirer) check(ctx context.Context, id domain.PaymentID) error {
	a, ok, err := x.queue.LatestAttempt(ctx, id)
	if err != nil {
		return fmt.Errorf("latest attempt: %w", err)
	}
	if ok && a.Status() != domain.AttemptRejected {
		if err := x.settle(ctx, &a); err != nil {
			return err
		}
	}
	// A no-op for a payment settle confirmed.
	expired, err := x.queue.Expire(ctx, id, x.clock.Now())
	if err != nil {
		return fmt.Errorf("expire: %w", err)
	}
	if expired {
		x.logger.InfoContext(ctx, "payment expired", "payment", id.String())
	}
	return nil
}

// settle applies a payment the provider reports, which either confirms the payment or
// posts the money as a divergence.
func (x *Expirer) settle(ctx context.Context, a *domain.PaymentAttempt) error {
	callCtx, cancel := context.WithTimeout(ctx, x.providerTimeout)
	result, err := x.provider.GetAttempt(callCtx, a.ID())
	cancel()
	switch {
	case errors.Is(err, ErrNotAtProvider):
		return nil
	case err != nil:
		return fmt.Errorf("lookup attempt %s: %w", a.ID(), err)
	case result.Outcome != Succeeded:
		return nil
	}
	u := result.Update()
	if _, _, err := x.queue.ApplyAttempt(ctx, a.ID(), &u, x.clock.Now()); err != nil {
		return fmt.Errorf("apply payment of attempt %s: %w", a.ID(), err)
	}
	return nil
}
