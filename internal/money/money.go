package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrOverflow         = errors.New("money: overflow")
)

type Money struct {
	amount   int64
	currency Currency
}

// New takes the amount in minor units: New(2000, USD) is US$ 20.00.
func New(amount int64, currency Currency) Money {
	return Money{amount: amount, currency: currency}
}

func Zero(currency Currency) Money {
	return Money{currency: currency}
}

func (m Money) Amount() int64 { return m.amount }

func (m Money) Currency() Currency { return m.currency }

func (m Money) IsZero() bool { return m.amount == 0 }

func (m Money) IsNegative() bool { return m.amount < 0 }

func (m Money) Add(other Money) (Money, error) {
	if err := m.sameCurrency(other); err != nil {
		return Money{}, err
	}
	if (other.amount > 0 && m.amount > math.MaxInt64-other.amount) ||
		(other.amount < 0 && m.amount < math.MinInt64-other.amount) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, other)
	}
	return Money{amount: m.amount + other.amount, currency: m.currency}, nil
}

func (m Money) Sub(other Money) (Money, error) {
	if err := m.sameCurrency(other); err != nil {
		return Money{}, err
	}
	if (other.amount > 0 && m.amount < math.MinInt64+other.amount) ||
		(other.amount < 0 && m.amount > math.MaxInt64+other.amount) {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrOverflow, m, other)
	}
	return Money{amount: m.amount - other.amount, currency: m.currency}, nil
}

func (m Money) Neg() (Money, error) {
	if m.amount == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: -(%s)", ErrOverflow, m)
	}
	return Money{amount: -m.amount, currency: m.currency}, nil
}

func (m Money) sameCurrency(other Money) error {
	if m.currency != other.currency {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	return nil
}

// String formats for logs and errors, e.g. "-20.50 USD". The API uses JSON in minor units.
func (m Money) String() string {
	digits := strconv.FormatInt(m.amount, 10)
	sign := ""
	if m.amount < 0 {
		sign, digits = "-", digits[1:]
	}

	exp := m.currency.exponent
	if exp == 0 {
		return fmt.Sprintf("%s%s %s", sign, digits, m.currency)
	}
	if len(digits) <= exp {
		digits = strings.Repeat("0", exp-len(digits)+1) + digits
	}
	cut := len(digits) - exp
	return fmt.Sprintf("%s%s.%s %s", sign, digits[:cut], digits[cut:], m.currency)
}
