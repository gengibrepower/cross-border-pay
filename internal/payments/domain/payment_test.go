package domain_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

func TestNewDraft(t *testing.T) {
	usd := func(n int64) money.Money { return money.New(n, money.USD) }
	tests := []struct {
		name      string
		method    string
		amount    money.Money
		reference string
		wantErr   error
	}{
		{"pix", "pix", usd(2000), "order-123", nil},
		{"100-character reference", "pix", usd(1), strings.Repeat("é", 100), nil},
		{"boleto until F8", "boleto", usd(2000), "o", domain.ErrUnsupportedMethod},
		{"card until F8", "card", usd(2000), "o", domain.ErrUnsupportedMethod},
		{"unknown method", "PIX", usd(2000), "o", domain.ErrUnknownMethod},
		{"charged in BRL", "pix", money.New(2000, money.BRL), "o", domain.ErrUnsupportedCurrency},
		{"zero", "pix", usd(0), "o", domain.ErrInvalidAmount},
		{"negative", "pix", usd(-1), "o", domain.ErrInvalidAmount},
		{"empty reference", "pix", usd(1), "", domain.ErrInvalidReference},
		{"101-character reference", "pix", usd(1), strings.Repeat("a", 101), domain.ErrInvalidReference},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := domain.NewDraft(domain.MerchantID{1}, tt.method, tt.amount, tt.reference)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewDraft() error = %v, want %v", err, tt.wantErr)
			}
			if err == nil && (d.Method() != domain.Pix || d.Amount() != tt.amount || d.Reference() != tt.reference) {
				t.Errorf("NewDraft() = %+v", d)
			}
		})
	}
}

func TestPaymentIDRoundTrip(t *testing.T) {
	id := domain.PaymentID{0x01, 0x9a, 0x2b, 15: 0xff}
	s := id.String()
	if !strings.HasPrefix(s, "pay_") || len(s) != len("pay_")+26 {
		t.Fatalf("String() = %q, want pay_ and 26 characters", s)
	}
	got, err := domain.ParsePaymentID(s)
	if err != nil || got != id {
		t.Errorf("ParsePaymentID(%q) = %v, %v; want %v", s, got, err, id)
	}
}

// UUIDv7 bytes sort by creation time; the public IDs must sort the same way.
func TestPaymentIDsSortLikeTheirBytes(t *testing.T) {
	ids := []domain.PaymentID{{0x01}, {0x01, 0x01}, {0x02}, {0x7f, 15: 1}, {0x80}, {0xff, 0xff}}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	if !slices.IsSorted(strs) {
		t.Errorf("public IDs out of order: %v", strs)
	}
}

func TestParsePaymentIDRejects(t *testing.T) {
	valid := domain.PaymentID{1}.String()
	for _, s := range []string{
		"",
		strings.TrimPrefix(valid, "pay_"),
		"att_" + strings.TrimPrefix(valid, "pay_"),
		valid[:len(valid)-1],
		valid + "0",
		strings.ToUpper(valid),
		"pay_" + strings.Repeat("u", 26), // u is not in Crockford base32
	} {
		if _, err := domain.ParsePaymentID(s); !errors.Is(err, domain.ErrInvalidPaymentID) {
			t.Errorf("ParsePaymentID(%q) error = %v, want %v", s, err, domain.ErrInvalidPaymentID)
		}
	}
}
