package domain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// Section 3.3: US$ 20.00 at 5.0000 with 1.5% spread and 3.0% fee.
func TestQuoteGameStore(t *testing.T) {
	q, err := domain.NewQuote(money.New(2000, money.USD), mustRate(t, "5"), domain.DefaultPricing, t0, t0.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	usd := func(n int64) money.Money { return money.New(n, money.USD) }
	checks := []struct {
		name      string
		got, want money.Money
	}{
		{"payer amount", q.PayerAmount(), money.New(10150, money.BRL)},
		{"converted", q.Converted(), usd(2030)},
		{"spread", q.Spread(), usd(30)},
		{"rounding", q.Rounding(), usd(0)},
		{"fee", q.Fee(), usd(60)},
		{"merchant net", q.MerchantNet(), usd(1940)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// Each part rounds on its own, so they can disagree by a cent; Rounding takes it.
func TestQuoteRoundingAbsorbsDisagreement(t *testing.T) {
	rate := mustRate(t, "4.9873")
	tests := []struct {
		amount, converted, spread, rounding int64
		payer                               int64
	}{
		// 2.98 × 4.9873 × 1.015 = 15.085… → R$ 15.09; back 3.0256… → 3.03; spread 0.0447 → 0.04.
		{amount: 298, payer: 1509, converted: 303, spread: 4, rounding: 1},
		// 0.34 × 4.9873 × 1.015 = 1.7211… → R$ 1.72; back 0.3448… → 0.34; spread 0.0051 → 0.01.
		{amount: 34, payer: 172, converted: 34, spread: 1, rounding: -1},
	}
	for _, tt := range tests {
		q, err := domain.NewQuote(money.New(tt.amount, money.USD), rate, domain.DefaultPricing, t0, t0.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		got := []int64{q.PayerAmount().Amount(), q.Converted().Amount(), q.Spread().Amount(), q.Rounding().Amount()}
		want := []int64{tt.payer, tt.converted, tt.spread, tt.rounding}
		if !slices.Equal(got, want) {
			t.Errorf("amount %d: payer, converted, spread, rounding = %v, want %v", tt.amount, got, want)
		}
	}
}

func TestNewQuoteRejects(t *testing.T) {
	rate := mustRate(t, "5")
	tests := []struct {
		name      string
		amount    money.Money
		pricing   domain.Pricing
		expiresAt time.Time
	}{
		{"amount in quote currency", money.New(2000, money.BRL), domain.DefaultPricing, t0.Add(time.Minute)},
		{"zero amount", money.New(0, money.USD), domain.DefaultPricing, t0.Add(time.Minute)},
		{"negative amount", money.New(-1, money.USD), domain.DefaultPricing, t0.Add(time.Minute)},
		{"expires now", money.New(2000, money.USD), domain.DefaultPricing, t0},
		{"expired", money.New(2000, money.USD), domain.DefaultPricing, t0.Add(-time.Second)},
		{"negative spread", money.New(2000, money.USD), domain.Pricing{Spread: -1}, t0.Add(time.Minute)},
		{"fee over 100%", money.New(2000, money.USD), domain.Pricing{Fee: 10_001}, t0.Add(time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := domain.NewQuote(tt.amount, rate, tt.pricing, t0, tt.expiresAt)
			if !errors.Is(err, domain.ErrInvalidQuote) {
				t.Errorf("NewQuote() error = %v, want %v", err, domain.ErrInvalidQuote)
			}
		})
	}
}

func TestQuoteExpired(t *testing.T) {
	q, err := domain.NewQuote(money.New(2000, money.USD), mustRate(t, "5"), domain.DefaultPricing, t0, t0.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		at   time.Time
		want bool
	}{
		{t0, false},
		{t0.Add(30*time.Minute - time.Microsecond), false},
		{t0.Add(30 * time.Minute), true},
		{t0.Add(time.Hour), true},
	}
	for _, tt := range tests {
		if got := q.Expired(tt.at); got != tt.want {
			t.Errorf("Expired(%s) = %v, want %v", tt.at, got, tt.want)
		}
	}
}

func drawQuote(t *rapid.T, minBaseRate int64) domain.Quote {
	rate, err := domain.NewRate(money.USD, money.BRL, rapid.Int64Range(minBaseRate, maxRate).Draw(t, "rate"))
	if err != nil {
		t.Fatal(err)
	}
	pricing := domain.Pricing{
		Spread: domain.BasisPoints(rapid.Int64Range(0, 2_000).Draw(t, "spread")),
		Fee:    domain.BasisPoints(rapid.Int64Range(0, 2_000).Draw(t, "fee")),
	}
	amount := money.New(rapid.Int64Range(1, maxAmount).Draw(t, "amount"), money.USD)
	q, err := domain.NewQuote(amount, rate, pricing, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// F1 done criterion: every conversion closes at zero counting the rounding account.
// These are the USD legs of the confirmation in section 3.3.
func TestQuoteUSDLegsSumToZero(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		q := drawQuote(t, minRate)
		sum := -q.Converted().Amount() + q.MerchantNet().Amount() + q.Fee().Amount() +
			q.Spread().Amount() + q.Rounding().Amount()
		if sum != 0 {
			t.Fatalf("USD legs sum to %d: %+v", sum, q)
		}
	})
}

// With base rate ≥ 1 each rounding is off by at most half a cent, so the parts can only
// disagree by one cent.
func TestQuoteRoundingIsAtMostOneCent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		q := drawQuote(t, rateScale)
		if r := q.Rounding().Amount(); r < -1 || r > 1 {
			t.Fatalf("Rounding() = %v for %v at %v", q.Rounding(), q.Amount(), q.BaseRate())
		}
	})
}
