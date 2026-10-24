package money_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

func TestMarshalJSON(t *testing.T) {
	got, err := json.Marshal(money.New(2000, money.USD))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"value":2000,"currency":"USD"}`; string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}

	if _, err := json.Marshal(money.Money{}); !errors.Is(err, money.ErrUnknownCurrency) {
		t.Errorf("Marshal(zero Money) error = %v, want %v", err, money.ErrUnknownCurrency)
	}
}

func TestUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    money.Money
		wantErr error
	}{
		{name: "valid", in: `{"value": 2000, "currency": "USD"}`, want: money.New(2000, money.USD)},
		{name: "negative", in: `{"value": -60, "currency": "USD"}`, want: money.New(-60, money.USD)},
		{name: "field order", in: `{"currency": "BRL", "value": 10150}`, want: money.New(10150, money.BRL)},
		{name: "decimal", in: `{"value": 20.5, "currency": "USD"}`, wantErr: money.ErrInvalidAmount},
		{name: "integral decimal", in: `{"value": 2000.0, "currency": "USD"}`, wantErr: money.ErrInvalidAmount},
		{name: "exponent", in: `{"value": 2e3, "currency": "USD"}`, wantErr: money.ErrInvalidAmount},
		{name: "string value", in: `{"value": "2000", "currency": "USD"}`, wantErr: money.ErrInvalidAmount},
		{name: "null value", in: `{"value": null, "currency": "USD"}`, wantErr: money.ErrInvalidAmount},
		{name: "missing value", in: `{"currency": "USD"}`, wantErr: money.ErrInvalidAmount},
		{name: "beyond int64", in: `{"value": 9223372036854775808, "currency": "USD"}`, wantErr: money.ErrInvalidAmount},
		{name: "missing currency", in: `{"value": 2000}`, wantErr: money.ErrUnknownCurrency},
		{name: "unknown currency", in: `{"value": 2000, "currency": "EUR"}`, wantErr: money.ErrUnknownCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got money.Money
			err := json.Unmarshal([]byte(tt.in), &got)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Unmarshal(%s) error = %v, want %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Unmarshal(%s) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestUnmarshalJSONRejectsUnknownFields(t *testing.T) {
	var m money.Money
	if err := json.Unmarshal([]byte(`{"value": 1, "currency": "USD", "amount": 1}`), &m); err == nil {
		t.Error("Unmarshal() accepted an unknown field")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	in := money.New(-10150, money.BRL)
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out money.Money
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round trip = %v, want %v", out, in)
	}
}
