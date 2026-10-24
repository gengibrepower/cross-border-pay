package app_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/fx/app"
	"github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newQuoter(t *testing.T) (*app.Quoter, *clock.Fake) {
	t.Helper()
	usdbrl, err := domain.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(t0)
	return app.NewQuoter(adapters.NewFixedRates(usdbrl), clk, domain.DefaultPricing), clk
}

func TestQuoterLocksRateForValidity(t *testing.T) {
	quoter, clk := newQuoter(t)

	q, err := quoter.Quote(t.Context(), money.New(2000, money.USD), money.BRL, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := q.PayerAmount(), money.New(10150, money.BRL); got != want {
		t.Errorf("PayerAmount() = %v, want %v", got, want)
	}
	if !q.CreatedAt().Equal(t0) || !q.ExpiresAt().Equal(t0.Add(30*time.Minute)) {
		t.Errorf("valid from %s to %s, want %s plus 30m", q.CreatedAt(), q.ExpiresAt(), t0)
	}

	clk.Advance(30 * time.Minute)
	if !q.Expired(clk.Now()) {
		t.Error("quote not expired after 30m")
	}
}

func TestQuoterErrors(t *testing.T) {
	quoter, _ := newQuoter(t)
	tests := []struct {
		name     string
		amount   money.Money
		payer    money.Currency
		validFor time.Duration
		wantErr  error
	}{
		{"pair not configured", money.New(2000, money.BRL), money.USD, time.Minute, domain.ErrRateUnavailable},
		{"no validity", money.New(2000, money.USD), money.BRL, 0, domain.ErrInvalidQuote},
		{"zero amount", money.New(0, money.USD), money.BRL, time.Minute, domain.ErrInvalidQuote},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := quoter.Quote(t.Context(), tt.amount, tt.payer, tt.validFor)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Quote() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
