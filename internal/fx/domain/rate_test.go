package domain_test

import (
	"errors"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

func TestParseRate(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "5.0750", want: 507_500_000},
		{in: "5", want: 500_000_000},
		{in: "0.00000001", want: 1},
		{in: "0.5", want: 50_000_000},
		{in: "92233720368", want: 9_223_372_036_800_000_000},
		{in: "92233720369", wantErr: true},
		{in: "5.123456789", wantErr: true},
		{in: "0", wantErr: true},
		{in: "0.00000000", wantErr: true},
		{in: "-5", wantErr: true},
		{in: "+5", wantErr: true},
		{in: "5.", want: 500_000_000},
		{in: ".5", wantErr: true},
		{in: "5,07", wantErr: true},
		{in: "1e2", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := domain.ParseRate(money.USD, money.BRL, tt.in)
			if tt.wantErr {
				if !errors.Is(err, domain.ErrInvalidRate) {
					t.Fatalf("ParseRate(%q) error = %v, want %v", tt.in, err, domain.ErrInvalidRate)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRate(%q) error = %v", tt.in, err)
			}
			if got.Value() != tt.want {
				t.Errorf("ParseRate(%q) = %d, want %d", tt.in, got.Value(), tt.want)
			}
		})
	}
}

func TestNewRateRejectsBadPairs(t *testing.T) {
	pairs := []struct{ base, quote money.Currency }{
		{money.USD, money.USD},
		{money.Currency{}, money.BRL},
		{money.USD, money.Currency{}},
	}
	for _, p := range pairs {
		if _, err := domain.NewRate(p.base, p.quote, rateScale); !errors.Is(err, domain.ErrInvalidRate) {
			t.Errorf("NewRate(%v, %v) error = %v, want %v", p.base, p.quote, err, domain.ErrInvalidRate)
		}
	}
}

func TestRateString(t *testing.T) {
	r := mustRate(t, "5.075")
	if got, want := r.String(), "USD/BRL 5.07500000"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

const rateScale = 100_000_000

func mustRate(t testing.TB, s string) domain.Rate {
	t.Helper()
	r, err := domain.ParseRate(money.USD, money.BRL, s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
