package domain_test

import (
	"errors"
	"math"
	"math/big"
	"testing"

	"pgregory.net/rapid"

	"github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

func TestConvert(t *testing.T) {
	usd := func(n int64) money.Money { return money.New(n, money.USD) }
	brl := func(n int64) money.Money { return money.New(n, money.BRL) }
	tests := []struct {
		name    string
		in      money.Money
		rate    string
		want    money.Money
		wantErr error
	}{
		{name: "GameStore payer amount", in: usd(2000), rate: "5.075", want: brl(10150)},
		{name: "GameStore at base rate", in: brl(10150), rate: "5", want: usd(2030)},
		{name: "round down", in: usd(1), rate: "0.4", want: brl(0)},
		{name: "round up", in: usd(1), rate: "0.6", want: brl(1)},
		{name: "tie to even 0", in: usd(1), rate: "0.5", want: brl(0)},
		{name: "tie to even 2", in: usd(3), rate: "0.5", want: brl(2)},
		{name: "tie to even 2 again", in: usd(5), rate: "0.5", want: brl(2)},
		{name: "negative tie", in: usd(-3), rate: "0.5", want: brl(-2)},
		{name: "negative round", in: usd(-1), rate: "0.6", want: brl(-1)},
		{name: "divide tie", in: brl(1), rate: "2", want: usd(0)},
		{name: "divide tie up", in: brl(3), rate: "2", want: usd(2)},
		{name: "intermediate beyond int64", in: usd(math.MaxInt64 / 2), rate: "2.5", wantErr: money.ErrOverflow},
		{name: "large but fits", in: usd(1_000_000_000_000_000), rate: "5.075", want: brl(5_075_000_000_000_000)},
		{name: "wrong currency", in: money.New(1, money.Currency{}), rate: "5", wantErr: money.ErrCurrencyMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := domain.Convert(tt.in, mustRate(t, tt.rate))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Convert(%v) error = %v, want %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Convert(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestConvertZeroRate(t *testing.T) {
	if _, err := domain.Convert(money.New(1, money.USD), domain.Rate{}); !errors.Is(err, domain.ErrInvalidRate) {
		t.Errorf("Convert with zero Rate error = %v, want %v", err, domain.ErrInvalidRate)
	}
}

// Realistic bounds keep conversions inside int64; overflow has its own table cases.
const (
	maxAmount = 1_000_000_000_000 // 10 billion in major units
	minRate   = rateScale / 100   // 0.01
	maxRate   = 1_000 * rateScale // 1000
)

func drawRate(t *rapid.T) domain.Rate {
	r, err := domain.NewRate(money.USD, money.BRL, rapid.Int64Range(minRate, maxRate).Draw(t, "rate"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func drawMoney(t *rapid.T, c money.Currency) money.Money {
	return money.New(rapid.Int64Range(-maxAmount, maxAmount).Draw(t, "amount"), c)
}

func TestConvertIsNearestHalfEven(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := drawRate(t)
		in := drawMoney(t, money.USD)
		got, err := domain.Convert(in, r)
		if err != nil {
			t.Fatal(err)
		}

		// exact = in × rate / 10^8; |got − exact| ≤ ½ ⟺ |got·10^8 − in·rate|·2 ≤ 10^8.
		exact := new(big.Int).Mul(big.NewInt(in.Amount()), big.NewInt(r.Value()))
		diff := new(big.Int).Sub(new(big.Int).Mul(big.NewInt(got.Amount()), big.NewInt(rateScale)), exact)
		twice := new(big.Int).Lsh(new(big.Int).Abs(diff), 1)
		switch twice.Cmp(big.NewInt(rateScale)) {
		case 1:
			t.Fatalf("Convert(%v, %v) = %v is more than half a cent away", in, r, got)
		case 0:
			if got.Amount()%2 != 0 {
				t.Fatalf("Convert(%v, %v) = %v: tie not rounded to even", in, r, got)
			}
		}
	})
}

func TestConvertIsOddSymmetric(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := drawRate(t)
		in := drawMoney(t, money.BRL)
		neg, err := in.Neg()
		if err != nil {
			t.Fatal(err)
		}
		a, errA := domain.Convert(in, r)
		b, errB := domain.Convert(neg, r)
		if errA != nil || errB != nil {
			t.Fatal(errA, errB)
		}
		if a.Amount() != -b.Amount() {
			t.Fatalf("Convert(%v) = %v but Convert(%v) = %v", in, a, neg, b)
		}
	})
}

func TestConvertIsMonotonic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := drawRate(t)
		a := drawMoney(t, money.BRL)
		b := drawMoney(t, money.BRL)
		if a.Amount() > b.Amount() {
			a, b = b, a
		}
		ca, errA := domain.Convert(a, r)
		cb, errB := domain.Convert(b, r)
		if errA != nil || errB != nil {
			t.Fatal(errA, errB)
		}
		if ca.Amount() > cb.Amount() {
			t.Fatalf("%v ≤ %v but Convert gives %v > %v", a, b, ca, cb)
		}
	})
}

// With rate ≥ 1 the base→quote step loses less than half a base cent, so converting back
// recovers the original exactly.
func TestConvertRoundTripsWhenRateAtLeastOne(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r, err := domain.NewRate(money.USD, money.BRL, rapid.Int64Range(rateScale, maxRate).Draw(t, "rate"))
		if err != nil {
			t.Fatal(err)
		}
		in := drawMoney(t, money.USD)
		there, err := domain.Convert(in, r)
		if err != nil {
			t.Fatal(err)
		}
		back, err := domain.Convert(there, r)
		if err != nil {
			t.Fatal(err)
		}
		if back != in {
			t.Fatalf("%v → %v → %v at %v", in, there, back, r)
		}
	})
}
