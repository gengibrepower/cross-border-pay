package domain

import (
	"errors"
	"fmt"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

var ErrCaptureExceedsAuthorization = errors.New("payments: capture exceeds the authorized amount")

// PriceCapture prices a partial capture at the locked rate and pricing, as if the
// merchant had asked for that amount from the start: the fee is charged on what was
// captured, never on what was only held, and refunds, priced from this quote, stop at
// it (invariant 8).
func PriceCapture(q *fx.Quote, amount money.Money) (fx.Quote, error) {
	if amount.Currency() != q.Amount().Currency() {
		return fx.Quote{}, fmt.Errorf("payments: capture %s: %w: the payment is in %s", amount, money.ErrCurrencyMismatch, q.Amount().Currency())
	}
	if amount.Amount() <= 0 {
		return fx.Quote{}, fmt.Errorf("%w: got %s", ErrInvalidAmount, amount)
	}
	if amount.Amount() > q.Amount().Amount() {
		return fx.Quote{}, fmt.Errorf("%w: %s of %s", ErrCaptureExceedsAuthorization, amount, q.Amount())
	}
	if amount == q.Amount() {
		return *q, nil
	}
	captured, err := fx.NewQuote(amount, q.BaseRate(), q.Pricing(), q.CreatedAt(), q.ExpiresAt())
	if err != nil {
		return fx.Quote{}, fmt.Errorf("payments: price capture of %s: %w", amount, err)
	}
	return captured, nil
}
