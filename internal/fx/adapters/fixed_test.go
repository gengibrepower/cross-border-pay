package adapters_test

import (
	"errors"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

func TestFixedRates(t *testing.T) {
	usdbrl, err := domain.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		t.Fatal(err)
	}
	rates := adapters.NewFixedRates(usdbrl)

	got, err := rates.Rate(t.Context(), money.USD, money.BRL)
	if err != nil || got != usdbrl {
		t.Errorf("Rate(USD, BRL) = %v, %v; want %v", got, err, usdbrl)
	}

	if _, err := rates.Rate(t.Context(), money.BRL, money.USD); !errors.Is(err, domain.ErrRateUnavailable) {
		t.Errorf("Rate(BRL, USD) error = %v, want %v: the inverse pair is not configured", err, domain.ErrRateUnavailable)
	}
}
