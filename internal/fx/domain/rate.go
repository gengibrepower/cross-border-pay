package domain

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

const (
	rateDecimals = 8
	rateScale    = 100_000_000
)

var ErrInvalidRate = errors.New("fx: invalid rate")

// Rate is how many units of quote one unit of base buys, scaled by 10^8:
// USD/BRL at 5.0750 is Rate{base: USD, quote: BRL, value: 507_500_000}.
type Rate struct {
	base  money.Currency
	quote money.Currency
	value int64
}

func NewRate(base, quote money.Currency, value int64) (Rate, error) {
	if base == (money.Currency{}) || quote == (money.Currency{}) || base == quote {
		return Rate{}, fmt.Errorf("%w: pair %s/%s", ErrInvalidRate, base, quote)
	}
	if value <= 0 {
		return Rate{}, fmt.Errorf("%w: value %d must be positive", ErrInvalidRate, value)
	}
	return Rate{base: base, quote: quote, value: value}, nil
}

// ParseRate reads a plain decimal such as "5.0750". More than 8 decimal places is an
// error rather than a silent rounding, because rates come from config and must be exact.
func ParseRate(base, quote money.Currency, s string) (Rate, error) {
	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" || len(fracPart) > rateDecimals || !digitsOnly(intPart) || !digitsOnly(fracPart) {
		return Rate{}, fmt.Errorf("%w: %q is not a decimal with up to %d places", ErrInvalidRate, s, rateDecimals)
	}

	units, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil || units > math.MaxInt64/rateScale {
		return Rate{}, fmt.Errorf("%w: %q is too large", ErrInvalidRate, s)
	}
	var frac int64
	if fracPart != "" {
		padded := fracPart + strings.Repeat("0", rateDecimals-len(fracPart))
		if frac, err = strconv.ParseInt(padded, 10, 64); err != nil {
			return Rate{}, fmt.Errorf("%w: %q: %w", ErrInvalidRate, s, err)
		}
	}
	return NewRate(base, quote, units*rateScale+frac)
}

func digitsOnly(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (r Rate) Base() money.Currency { return r.base }

func (r Rate) Quote() money.Currency { return r.quote }

func (r Rate) Value() int64 { return r.value }

func (r Rate) String() string {
	return fmt.Sprintf("%s/%s %s", r.base, r.quote, r.Decimal())
}

// Decimal is the value with all 8 places, e.g. "5.07500000", for APIs and logs.
func (r Rate) Decimal() string {
	return fmt.Sprintf("%d.%08d", r.value/rateScale, r.value%rateScale)
}
