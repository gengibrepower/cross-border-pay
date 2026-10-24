package app

import (
	"context"
	"fmt"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

type RateSource interface {
	Rate(ctx context.Context, base, quote money.Currency) (domain.Rate, error)
}

type Clock interface {
	Now() time.Time
}

type Quoter struct {
	rates   RateSource
	clock   Clock
	pricing domain.Pricing
}

func NewQuoter(rates RateSource, clock Clock, pricing domain.Pricing) *Quoter {
	return &Quoter{rates: rates, clock: clock, pricing: pricing}
}

// Quote locks a rate for validFor. The caller picks validFor by payment method
// (decision 44), which fx does not know about.
func (q *Quoter) Quote(ctx context.Context, amount money.Money, payer money.Currency, validFor time.Duration) (domain.Quote, error) {
	rate, err := q.rates.Rate(ctx, amount.Currency(), payer)
	if err != nil {
		return domain.Quote{}, fmt.Errorf("fx: quote %s in %s: %w", amount, payer, err)
	}
	now := q.clock.Now()
	quote, err := domain.NewQuote(amount, rate, q.pricing, now, now.Add(validFor))
	if err != nil {
		return domain.Quote{}, fmt.Errorf("fx: quote %s in %s: %w", amount, payer, err)
	}
	return quote, nil
}
