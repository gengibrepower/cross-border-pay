package domain_test

import (
	"errors"
	"testing"
	"time"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

func authorizedQuote(t *testing.T) fx.Quote {
	t.Helper()
	rate, err := fx.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	q, err := fx.NewQuote(money.New(2000, money.USD), rate, fx.DefaultPricing, now, now.Add(domain.CardAuthorizationValidity))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// The GameStore authorizes US$ 20.00 and ships US$ 15.00 of it: the payer pays
// R$ 76.12 (76.125 rounded half-even) instead of R$ 101.50, and the 3% fee is on the
// US$ 15.00.
func TestPriceCapture(t *testing.T) {
	q := authorizedQuote(t)
	whole, err := domain.PriceCapture(&q, money.New(2000, money.USD))
	if err != nil || whole != q {
		t.Errorf("PriceCapture(whole) = %+v, %v; want the payment's own quote", whole, err)
	}
	part, err := domain.PriceCapture(&q, money.New(1500, money.USD))
	if err != nil {
		t.Fatal(err)
	}
	if part.PayerAmount() != money.New(7612, money.BRL) || part.Fee() != money.New(45, money.USD) || part.MerchantNet() != money.New(1455, money.USD) {
		t.Errorf("part = payer %s, fee %s, net %s; want R$ 76.12, US$ 0.45, US$ 14.55", part.PayerAmount(), part.Fee(), part.MerchantNet())
	}
	if part.BaseRate() != q.BaseRate() || part.Pricing() != q.Pricing() || !part.CreatedAt().Equal(q.CreatedAt()) || !part.ExpiresAt().Equal(q.ExpiresAt()) {
		t.Errorf("part was priced anew: %+v", part)
	}
	for _, tc := range []struct {
		amount money.Money
		want   error
	}{
		{money.New(2001, money.USD), domain.ErrCaptureExceedsAuthorization},
		{money.New(0, money.USD), domain.ErrInvalidAmount},
		{money.New(-1, money.USD), domain.ErrInvalidAmount},
		{money.New(1000, money.BRL), money.ErrCurrencyMismatch},
	} {
		if _, err := domain.PriceCapture(&q, tc.amount); !errors.Is(err, tc.want) {
			t.Errorf("PriceCapture(%s) error = %v, want %v", tc.amount, err, tc.want)
		}
	}
}
