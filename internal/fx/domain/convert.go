package domain

import (
	"fmt"
	"math/big"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

// Convert applies the rate in either direction: base amounts are multiplied by it,
// quote amounts are divided. The result is rounded half-even (decision 11).
func Convert(m money.Money, r Rate) (money.Money, error) {
	if r.value <= 0 {
		return money.Money{}, fmt.Errorf("%w: zero Rate", ErrInvalidRate)
	}
	var (
		num, den = big.NewInt(m.Amount()), big.NewInt(rateScale)
		target   money.Currency
	)
	switch m.Currency() {
	case r.base:
		num.Mul(num, big.NewInt(r.value))
		target = r.quote
	case r.quote:
		num.Mul(num, big.NewInt(rateScale))
		den.SetInt64(r.value)
		target = r.base
	default:
		return money.Money{}, fmt.Errorf("fx: convert %s with %s: %w", m, r, money.ErrCurrencyMismatch)
	}
	scaleExponents(num, den, m.Currency(), target)

	result := divRoundHalfEven(num, den)
	if !result.IsInt64() {
		return money.Money{}, fmt.Errorf("fx: convert %s with %s: %w", m, r, money.ErrOverflow)
	}
	return money.New(result.Int64(), target), nil
}

// scaleExponents adjusts for currencies with different minor-unit digits. BRL and USD
// both use 2, so today this multiplies by 1.
func scaleExponents(num, den *big.Int, from, to money.Currency) {
	ten := big.NewInt(10)
	num.Mul(num, new(big.Int).Exp(ten, big.NewInt(int64(to.Exponent())), nil))
	den.Mul(den, new(big.Int).Exp(ten, big.NewInt(int64(from.Exponent())), nil))
}

// divRoundHalfEven is the only rounding rule in the system (section 3.2). den must be
// positive.
func divRoundHalfEven(num, den *big.Int) *big.Int {
	q, rem := new(big.Int).DivMod(num, den, new(big.Int))
	twiceRem := new(big.Int).Lsh(rem, 1)
	switch twiceRem.Cmp(den) {
	case 1:
		q.Add(q, big.NewInt(1))
	case 0:
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}
