package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// DueRefund is a claimed refund with the number of lookups that already failed.
type DueRefund struct {
	Refund domain.Refund
	Checks int
}

type RefundQueue interface {
	// ClaimDueRefunds takes up to limit refunds whose check is due at now and moves
	// their next check to leaseUntil, so no other worker takes them meanwhile.
	ClaimDueRefunds(ctx context.Context, now, leaseUntil time.Time, limit int) ([]DueRefund, error)
	// RescheduleRefund records one more failed lookup of an unknown refund.
	RescheduleRefund(ctx context.Context, id domain.RefundID, next time.Time) error
	// EscalateRefund takes the refund out of the queue for a person to resolve; its
	// hold stays until then.
	EscalateRefund(ctx context.Context, id domain.RefundID, now time.Time) error
}

// RefundResolver finishes the refunds a request left behind (decision 17, section
// 4.3): it sends a created one, which the provider certainly never got, and looks up
// a sent or unknown one, never sending it again. Without it a refund whose request
// died or timed out would keep the merchant's money held until a retry came.
type RefundResolver struct {
	refunder *Refunder
	queue    RefundQueue
}

func NewRefundResolver(refunder *Refunder, queue RefundQueue) *RefundResolver {
	return &RefundResolver{refunder: refunder, queue: queue}
}

// Resolve checks one batch of due refunds and returns how many it claimed. One refund
// failing does not stop the others; their errors are joined. Without today's rate
// nothing is claimed: a completion is posted at it.
func (r *RefundResolver) Resolve(ctx context.Context, limit int) (int, error) {
	rate, err := r.refunder.rates.Rate(ctx, domain.ChargeCurrency, money.BRL)
	if err != nil {
		return 0, fmt.Errorf("payments: resolve refunds: %w", err)
	}
	now := r.refunder.clock.Now()
	due, err := r.queue.ClaimDueRefunds(ctx, now, now.Add(ClaimLease), limit)
	if err != nil {
		return 0, fmt.Errorf("payments: claim refunds: %w", err)
	}
	var errs []error
	for i := range due {
		if ctx.Err() != nil {
			break
		}
		if err := r.check(ctx, &due[i], rate); err != nil {
			errs = append(errs, fmt.Errorf("payments: check refund %s: %w", due[i].Refund.ID(), err))
		}
	}
	return len(due), errors.Join(errs...)
}

func (r *RefundResolver) check(ctx context.Context, d *DueRefund, rate fx.Rate) error {
	f := r.refunder
	refund := &d.Refund
	if refund.Status() == domain.RefundCreated {
		if _, err := f.send(ctx, refund, rate); err != nil {
			return fmt.Errorf("send: %w", err)
		}
		return nil
	}

	now := f.clock.Now()
	if now.Sub(refund.CreatedAt()) >= domain.ReviewAfter {
		f.logger.ErrorContext(ctx, "refund still ambiguous after the review limit; a person must resolve it",
			"refund", refund.ID().String(), "payment", refund.Payment().String(), "status", string(refund.Status()), "checks", d.Checks)
		if err := r.queue.EscalateRefund(ctx, refund.ID(), now); err != nil {
			return fmt.Errorf("escalate: %w", err)
		}
		return nil
	}

	callCtx, cancel := context.WithTimeout(ctx, f.providerTimeout)
	result, err := f.provider.GetRefund(callCtx, refund.ID())
	cancel()
	now = f.clock.Now()
	switch {
	case err == nil:
		u := result.Update()
		_, err := f.apply(ctx, refund, &u, rate)
		return err
	// The grace runs from the refund's last change, not its creation: a created refund
	// the resolver sent late may have reached the provider only a moment ago.
	case errors.Is(err, ErrNotAtProvider) && refund.Status() == domain.RefundUnknown && now.Sub(refund.UpdatedAt()) >= domain.NotFoundGrace:
		// Our call gave up long ago, so nothing can still reach the provider: the money
		// never went, and the hold goes back to the merchant.
		_, err := f.apply(ctx, refund, &domain.RefundUpdate{Event: domain.RejectRefund, Reason: "not at the provider"}, rate)
		return err
	default:
		f.logger.WarnContext(ctx, "refund lookup did not resolve it", "refund", refund.ID().String(), "checks", d.Checks+1, "error", err)
		if refund.Status() == domain.RefundSent {
			// A stale sent becomes unknown, whose first check is scheduled with it.
			_, err := f.apply(ctx, refund, &domain.RefundUpdate{Event: domain.RefundAmbiguous}, rate)
			return err
		}
		if err := r.queue.RescheduleRefund(ctx, refund.ID(), now.Add(domain.CheckBackoff(d.Checks+1))); err != nil {
			return fmt.Errorf("reschedule: %w", err)
		}
		return nil
	}
}
