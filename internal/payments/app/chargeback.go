package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

var ErrChargebackNotFound = errors.New("payments: chargeback not found")

// Disputer answers again the same way as it is. domain.ErrChargebackClosed is the
// provider refusing an answer the chargeback can no longer take.
type Disputer interface {
	AcceptChargeback(ctx context.Context, externalID string) (domain.ChargebackStatus, error)
	ContestChargeback(ctx context.Context, externalID, evidence string) (domain.ChargebackStatus, error)
}

type ChargebackStore interface {
	// GetChargeback returns ErrChargebackNotFound unless the merchant's payment has it.
	GetChargeback(ctx context.Context, merchant domain.MerchantID, id domain.ChargebackID) (domain.ChargebackCase, error)
	AnswerChargeback(ctx context.Context, merchant domain.MerchantID, id domain.ChargebackID, to domain.ChargebackStatus, evidence string, now time.Time) (domain.ChargebackCase, error)
}

// Chargebacks is the merchant's side of section 4.7. Neither answer takes an
// Idempotency-Key: the provider answers a repeated answer as it is, so a retry after a
// lost answer is safe.
type Chargebacks struct {
	store           ChargebackStore
	disputers       map[domain.ProviderName]Disputer
	clock           Clock
	logger          *slog.Logger
	providerTimeout time.Duration
}

func NewChargebacks(store ChargebackStore, disputers map[domain.ProviderName]Disputer, clock Clock, logger *slog.Logger, providerTimeout time.Duration) *Chargebacks {
	return &Chargebacks{store: store, disputers: disputers, clock: clock, logger: logger, providerTimeout: providerTimeout}
}

func (c *Chargebacks) Get(ctx context.Context, merchant domain.MerchantID, id domain.ChargebackID) (domain.ChargebackCase, error) {
	cb, err := c.store.GetChargeback(ctx, merchant, id)
	if err != nil {
		return domain.ChargebackCase{}, fmt.Errorf("payments: chargeback: %w", err)
	}
	return cb, nil
}

func (c *Chargebacks) Accept(ctx context.Context, merchant domain.MerchantID, id domain.ChargebackID) (domain.ChargebackCase, error) {
	return c.answer(ctx, merchant, id, domain.Lost, "", func(ctx context.Context, d Disputer, ext string) (domain.ChargebackStatus, error) {
		return d.AcceptChargeback(ctx, ext)
	})
}

// Contest leaves the chargeback under review until the provider's event decides it.
func (c *Chargebacks) Contest(ctx context.Context, merchant domain.MerchantID, id domain.ChargebackID, evidence string) (domain.ChargebackCase, error) {
	ev, err := domain.NewEvidence(evidence)
	if err != nil {
		return domain.ChargebackCase{}, fmt.Errorf("contest %s: %w", id, err)
	}
	return c.answer(ctx, merchant, id, domain.UnderReview, ev, func(ctx context.Context, d Disputer, ext string) (domain.ChargebackStatus, error) {
		return d.ContestChargeback(ctx, ext, ev)
	})
}

func (c *Chargebacks) answer(ctx context.Context, merchant domain.MerchantID, id domain.ChargebackID, want domain.ChargebackStatus, evidence string,
	call func(context.Context, Disputer, string) (domain.ChargebackStatus, error),
) (domain.ChargebackCase, error) {
	cb, err := c.store.GetChargeback(ctx, merchant, id)
	if err != nil {
		return domain.ChargebackCase{}, fmt.Errorf("payments: chargeback: %w", err)
	}
	switch {
	case cb.Status == want:
		return cb, nil
	case cb.Status != domain.NeedsResponse:
		return domain.ChargebackCase{}, fmt.Errorf("%w: it is %s", domain.ErrChargebackClosed, cb.Status)
	}
	d, ok := c.disputers[cb.Provider]
	if !ok {
		return domain.ChargebackCase{}, fmt.Errorf("payments: chargeback %s: %w: %s takes no answer", id, ErrUnknownProvider, cb.Provider)
	}
	// The provider may act on the answer: a client that gives up must not cut the call
	// or the write of what it said short.
	ctx = context.WithoutCancel(ctx)
	callCtx, cancel := context.WithTimeout(ctx, c.providerTimeout)
	status, err := call(callCtx, d, cb.ExternalID)
	cancel()
	if errors.Is(err, domain.ErrChargebackClosed) {
		return domain.ChargebackCase{}, fmt.Errorf("payments: chargeback %s: %w", id, err)
	}
	if err != nil {
		c.logger.WarnContext(ctx, "chargeback answer failed", "chargeback", id.String(), "error", err)
		return domain.ChargebackCase{}, fmt.Errorf("payments: chargeback %s: %w: %w", id, ErrProviderUnavailable, err)
	}
	stored, err := c.store.AnswerChargeback(ctx, merchant, id, status, evidence, c.clock.Now())
	if err != nil {
		return domain.ChargebackCase{}, fmt.Errorf("payments: chargeback %s: %w", id, err)
	}
	return stored, nil
}
