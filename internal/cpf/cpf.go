// Package cpf is decision 54. A CPF never prints whole: String and the log value are
// masked, and only Digits gives the number to whoever must send it on.
package cpf

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

var ErrInvalid = errors.New("cpf: invalid CPF")

const Length = 11

// CPF's zero value is not valid.
type CPF struct {
	digits string
}

// Parse refuses eleven equal digits: they pass the check digits but are not issued.
func Parse(s string) (CPF, error) {
	bare := strings.NewReplacer(".", "", "-", "").Replace(strings.TrimSpace(s))
	if len(bare) != Length {
		return CPF{}, fmt.Errorf("%w: a CPF has %d digits", ErrInvalid, Length)
	}
	for i := range Length {
		if bare[i] < '0' || bare[i] > '9' {
			return CPF{}, fmt.Errorf("%w: a CPF has only digits", ErrInvalid)
		}
	}
	if strings.Count(bare, bare[:1]) == Length {
		return CPF{}, fmt.Errorf("%w: eleven equal digits", ErrInvalid)
	}
	if bare[9] != checkDigit(bare[:9]) || bare[10] != checkDigit(bare[:10]) {
		return CPF{}, fmt.Errorf("%w: check digits do not match", ErrInvalid)
	}
	return CPF{digits: bare}, nil
}

// New is how test CPFs are made.
func New(base string) (CPF, error) {
	if len(base) != Length-2 {
		return CPF{}, fmt.Errorf("%w: a base has %d digits", ErrInvalid, Length-2)
	}
	withFirst := base + string(checkDigit(base))
	return Parse(withFirst + string(checkDigit(withFirst)))
}

// checkDigit is the Receita Federal's modulo 11.
func checkDigit(s string) byte {
	sum := 0
	for i := range len(s) {
		sum += int(s[i]-'0') * (len(s) + 1 - i)
	}
	r := sum % 11
	if r < 2 {
		return '0'
	}
	return byte('0' + 11 - r)
}

func (c CPF) IsZero() bool { return c.digits == "" }

func (c CPF) Digits() string { return c.digits }

// Masked is how Brazilian receipts print it.
func (c CPF) Masked() string {
	if c.IsZero() {
		return ""
	}
	return "***." + c.digits[3:6] + "." + c.digits[6:9] + "-**"
}

func (c CPF) String() string { return c.Masked() }

func (c CPF) LogValue() slog.Value { return slog.StringValue(c.Masked()) }
