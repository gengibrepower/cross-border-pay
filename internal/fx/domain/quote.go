package domain

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

var (
	ErrInvalidQuote    = errors.New("fx: invalid quote")
	ErrRateUnavailable = errors.New("fx: rate unavailable")
)

// BasisPoints are hundredths of a percent: 150 is 1.5%.
type BasisPoints int64

const fullBasisPoints = 10_000

type Pricing struct {
	Spread BasisPoints
	Fee    BasisPoints
}

// DefaultPricing is decision 43: 1.5% spread to the payer, 3.0% fee to the merchant.
var DefaultPricing = Pricing{Spread: 150, Fee: 300}

func (p Pricing) validate() error {
	if p.Spread < 0 || p.Spread > fullBasisPoints || p.Fee < 0 || p.Fee > fullBasisPoints {
		return fmt.Errorf("%w: pricing %+v outside 0..%d bps", ErrInvalidQuote, p, fullBasisPoints)
	}
	return nil
}

// Quote locks what the payer pays and how the converted amount splits. Each part has
// its own formula and rounding; Rounding absorbs the cents they disagree on, so that
// Converted = Amount + Spread + Rounding and MerchantNet + Fee = Amount, exactly.
type QuoteID [16]byte

type Quote struct {
	pricing     Pricing
	amount      money.Money
	baseRate    Rate
	payerAmount money.Money
	converted   money.Money
	spread      money.Money
	rounding    money.Money
	fee         money.Money
	merchantNet money.Money
	createdAt   time.Time
	expiresAt   time.Time
}

func NewQuote(amount money.Money, base Rate, p Pricing, now, expiresAt time.Time) (Quote, error) {
	if err := p.validate(); err != nil {
		return Quote{}, err
	}
	if amount.Currency() != base.base {
		return Quote{}, fmt.Errorf("%w: amount %s is not in %s", ErrInvalidQuote, amount, base.base)
	}
	if amount.Amount() <= 0 {
		return Quote{}, fmt.Errorf("%w: amount %s must be positive", ErrInvalidQuote, amount)
	}
	if !expiresAt.After(now) {
		return Quote{}, fmt.Errorf("%w: expires at %s, not after %s", ErrInvalidQuote, expiresAt, now)
	}

	q := Quote{pricing: p, amount: amount, baseRate: base, createdAt: now, expiresAt: expiresAt}
	if err := q.price(p); err != nil {
		return Quote{}, fmt.Errorf("%w: %w", ErrInvalidQuote, err)
	}
	return q, nil
}

func (q *Quote) price(p Pricing) error {
	var err error
	q.payerAmount, err = convertScaled(q.amount, q.baseRate,
		big.NewInt(int64(fullBasisPoints+p.Spread)), big.NewInt(fullBasisPoints))
	if err != nil {
		return fmt.Errorf("payer amount: %w", err)
	}
	if q.converted, err = Convert(q.payerAmount, q.baseRate); err != nil {
		return fmt.Errorf("converted: %w", err)
	}
	if q.spread, err = mulRatio(q.amount, int64(p.Spread), fullBasisPoints); err != nil {
		return fmt.Errorf("spread: %w", err)
	}
	if q.fee, err = mulRatio(q.amount, int64(p.Fee), fullBasisPoints); err != nil {
		return fmt.Errorf("fee: %w", err)
	}
	if q.merchantNet, err = q.amount.Sub(q.fee); err != nil {
		return fmt.Errorf("merchant net: %w", err)
	}

	excess, err := q.converted.Sub(q.amount)
	if err == nil {
		q.rounding, err = excess.Sub(q.spread)
	}
	if err != nil {
		return fmt.Errorf("rounding: %w", err)
	}
	return nil
}

func (q *Quote) Pricing() Pricing { return q.pricing }

// Amount is what the merchant charged, in the base currency.
func (q *Quote) Amount() money.Money { return q.amount }

func (q *Quote) BaseRate() Rate { return q.baseRate }

// PayerAmount is what the payer pays, in the quote currency, with the spread applied.
func (q *Quote) PayerAmount() money.Money { return q.payerAmount }

// Converted is PayerAmount back into the base currency at the base rate.
func (q *Quote) Converted() money.Money { return q.converted }

func (q *Quote) Spread() money.Money { return q.spread }

// Rounding may be negative: it is the cent the separate roundings disagree on.
func (q *Quote) Rounding() money.Money { return q.rounding }

func (q *Quote) Fee() money.Money { return q.fee }

func (q *Quote) MerchantNet() money.Money { return q.merchantNet }

func (q *Quote) CreatedAt() time.Time { return q.createdAt }

func (q *Quote) ExpiresAt() time.Time { return q.expiresAt }

func (q *Quote) Expired(now time.Time) bool { return !now.Before(q.expiresAt) }
