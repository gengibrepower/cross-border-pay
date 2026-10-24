package money_test

import (
	"errors"
	"math"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

func TestParseCurrency(t *testing.T) {
	tests := []struct {
		code    string
		want    money.Currency
		wantErr error
	}{
		{code: "BRL", want: money.BRL},
		{code: "USD", want: money.USD},
		{code: "usd", wantErr: money.ErrUnknownCurrency},
		{code: "EUR", wantErr: money.ErrUnknownCurrency},
		{code: "", wantErr: money.ErrUnknownCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			got, err := money.ParseCurrency(tt.code)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseCurrency(%q) error = %v, want %v", tt.code, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseCurrency(%q) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

func TestAddSub(t *testing.T) {
	usd := func(n int64) money.Money { return money.New(n, money.USD) }
	tests := []struct {
		name    string
		op      func(money.Money, money.Money) (money.Money, error)
		a, b    money.Money
		want    money.Money
		wantErr error
	}{
		{name: "add", op: money.Money.Add, a: usd(2000), b: usd(-60), want: usd(1940)},
		{name: "sub", op: money.Money.Sub, a: usd(2030), b: usd(2000), want: usd(30)},
		{name: "sub to negative", op: money.Money.Sub, a: usd(10), b: usd(25), want: usd(-15)},
		{name: "add max", op: money.Money.Add, a: usd(math.MaxInt64 - 1), b: usd(1), want: usd(math.MaxInt64)},
		{name: "add overflow", op: money.Money.Add, a: usd(math.MaxInt64), b: usd(1), wantErr: money.ErrOverflow},
		{name: "add underflow", op: money.Money.Add, a: usd(math.MinInt64), b: usd(-1), wantErr: money.ErrOverflow},
		{name: "sub overflow", op: money.Money.Sub, a: usd(math.MaxInt64), b: usd(-1), wantErr: money.ErrOverflow},
		{name: "sub underflow", op: money.Money.Sub, a: usd(math.MinInt64), b: usd(1), wantErr: money.ErrOverflow},
		{name: "sub min from negative", op: money.Money.Sub, a: usd(-1), b: usd(math.MinInt64), want: usd(math.MaxInt64)},
		{name: "add mismatch", op: money.Money.Add, a: usd(1), b: money.New(1, money.BRL), wantErr: money.ErrCurrencyMismatch},
		{name: "sub mismatch", op: money.Money.Sub, a: usd(1), b: money.New(1, money.BRL), wantErr: money.ErrCurrencyMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.op(tt.a, tt.b)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNeg(t *testing.T) {
	got, err := money.New(150, money.BRL).Neg()
	if err != nil || got != money.New(-150, money.BRL) {
		t.Errorf("Neg() = %v, %v; want -1.50 BRL", got, err)
	}

	if _, err := money.New(math.MinInt64, money.BRL).Neg(); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("Neg(MinInt64) error = %v, want %v", err, money.ErrOverflow)
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		m    money.Money
		want string
	}{
		{money.New(2000, money.USD), "20.00 USD"},
		{money.New(10150, money.BRL), "101.50 BRL"},
		{money.New(5, money.USD), "0.05 USD"},
		{money.New(-5, money.USD), "-0.05 USD"},
		{money.New(-2050, money.USD), "-20.50 USD"},
		{money.Zero(money.BRL), "0.00 BRL"},
		{money.New(math.MinInt64, money.USD), "-92233720368547758.08 USD"},
	}
	for _, tt := range tests {
		if got := tt.m.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}
