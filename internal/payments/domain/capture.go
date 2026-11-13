package domain

import (
	"errors"
	"fmt"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

// ErrCaptureExceedsAuthorization is a capture of more than the payment asked for,
// which is all the card was authorized for.
var ErrCaptureExceedsAuthorization = errors.New("payments: capture exceeds the authorized amount")

// PriceCapture is the quote of what a capture takes: the whole authorization is the
// payment's own quote, and a part is priced again at the same locked base rate and
// pricing, as if the merchant had asked for that amount from the start. The payer pays
// its BRL, the merchant gets its net, and the fee is charged on what was captured,
// never on what was only held. Refunds then stop at what was captured (invariant 8),
// because they are priced from this quote.
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
