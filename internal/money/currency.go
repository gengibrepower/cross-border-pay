package money

import (
	"errors"
	"fmt"
)

var ErrUnknownCurrency = errors.New("money: unknown currency")

type Currency struct {
	code     string
	exponent int
}

var (
	BRL = Currency{code: "BRL", exponent: 2}
	USD = Currency{code: "USD", exponent: 2}
)

var currencies = map[string]Currency{
	BRL.code: BRL,
	USD.code: USD,
}

func ParseCurrency(code string) (Currency, error) {
	c, ok := currencies[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, code)
	}
	return c, nil
}

func (c Currency) Code() string { return c.code }

// Exponent is the number of minor-unit digits: 2 means amounts are in cents.
func (c Currency) Exponent() int { return c.exponent }

func (c Currency) String() string { return c.code }

func (c Currency) MarshalText() ([]byte, error) {
	if c.code == "" {
		return nil, ErrUnknownCurrency
	}
	return []byte(c.code), nil
}

func (c *Currency) UnmarshalText(text []byte) error {
	parsed, err := ParseCurrency(string(text))
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}
