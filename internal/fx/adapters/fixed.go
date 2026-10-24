package adapters

import (
	"context"
	"fmt"

	"github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

type pair struct {
	base, quote money.Currency
}

// FixedRates is the configured RateSource of decision 12, until PTAX arrives.
type FixedRates struct {
	rates map[pair]domain.Rate
}

func NewFixedRates(rates ...domain.Rate) FixedRates {
	m := make(map[pair]domain.Rate, len(rates))
	for _, r := range rates {
		m[pair{r.Base(), r.Quote()}] = r
	}
	return FixedRates{rates: m}
}

func (f FixedRates) Rate(_ context.Context, base, quote money.Currency) (domain.Rate, error) {
	r, ok := f.rates[pair{base, quote}]
	if !ok {
		return domain.Rate{}, fmt.Errorf("%w: %s/%s", domain.ErrRateUnavailable, base, quote)
	}
	return r, nil
}
