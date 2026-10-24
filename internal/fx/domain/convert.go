package domain

import (
	"fmt"
	"math/big"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

// Convert applies the rate in either direction: base amounts are multiplied by it,
// quote amounts are divided. The result is rounded half-even (decision 11).
func Convert(m money.Money, r Rate) (money.Money, error) {
	return convertScaled(m, r, big.NewInt(1), big.NewInt(1))
}

// convertScaled is Convert(m, r) × num/den with a single rounding at the end, so that
// a spread on top of the rate does not round twice.
func convertScaled(m money.Money, r Rate, num, den *big.Int) (money.Money, error) {
	if r.value <= 0 {
		return money.Money{}, fmt.Errorf("%w: zero Rate", ErrInvalidRate)
	}
	num = new(big.Int).Mul(num, big.NewInt(m.Amount()))
	den = new(big.Int).Set(den)

	var target money.Currency
	switch m.Currency() {
	case r.base:
		num.Mul(num, big.NewInt(r.value))
		den.Mul(den, big.NewInt(rateScale))
		target = r.quote
	case r.quote:
		num.Mul(num, big.NewInt(rateScale))
		den.Mul(den, big.NewInt(r.value))
		target = r.base
	default:
		return money.Money{}, fmt.Errorf("fx: convert %s with %s: %w", m, r, money.ErrCurrencyMismatch)
	}
	scaleExponents(num, den, m.Currency(), target)

	result, err := toMoney(divRoundHalfEven(num, den), target)
	if err != nil {
		return money.Money{}, fmt.Errorf("fx: convert %s with %s: %w", m, r, err)
	}
	return result, nil
}

// mulRatio returns m × num/den rounded half-even, in m's currency.
func mulRatio(m money.Money, num, den int64) (money.Money, error) {
	n := new(big.Int).Mul(big.NewInt(m.Amount()), big.NewInt(num))
	return toMoney(divRoundHalfEven(n, big.NewInt(den)), m.Currency())
}

func toMoney(amount *big.Int, c money.Currency) (money.Money, error) {
	if !amount.IsInt64() {
		return money.Money{}, money.ErrOverflow
	}
	return money.New(amount.Int64(), c), nil
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
