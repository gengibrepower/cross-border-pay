package domain_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
		card      string
		manual    bool
		wantErr   error
	}{
		{"pix", "pix", usd(2000), "order-123", "", false, nil},
		{"100-character reference", "pix", usd(1), strings.Repeat("é", 100), "", false, nil},
		// Whether a method is offered is the providers' call, not the draft's.
		{"boleto", "boleto", usd(2000), "o", "", false, nil},
		{"card", "card", usd(2000), "o", "tok_approved", false, nil},
		{"card without a token", "card", usd(2000), "o", "", false, domain.ErrCardRequired},
		{"card number as token", "card", usd(2000), "o", "4111111111111111", false, domain.ErrInvalidCardToken},
		{"token with a space", "card", usd(2000), "o", "tok_a b", false, domain.ErrInvalidCardToken},
		{"token too long", "card", usd(2000), "o", "tok_" + strings.Repeat("a", 61), false, domain.ErrInvalidCardToken},
		{"bare prefix", "card", usd(2000), "o", "tok_", false, domain.ErrInvalidCardToken},
		{"60-character token", "card", usd(2000), "o", "tok_" + strings.Repeat("a", 60), false, nil},
		{"pix with a card", "pix", usd(2000), "o", "tok_approved", false, domain.ErrCardNotAllowed},
		{"manual card", "card", usd(2000), "o", "tok_approved", true, nil},
		{"manual capture on pix", "pix", usd(2000), "o", "", true, domain.ErrCardNotAllowed},
		{"unknown method", "PIX", usd(2000), "o", "", false, domain.ErrUnknownMethod},
		{"charged in BRL", "pix", money.New(2000, money.BRL), "o", "", false, domain.ErrUnsupportedCurrency},
		{"zero", "pix", usd(0), "o", "", false, domain.ErrInvalidAmount},
		{"negative", "pix", usd(-1), "o", "", false, domain.ErrInvalidAmount},
		{"empty reference", "pix", usd(1), "", "", false, domain.ErrInvalidReference},
		{"101-character reference", "pix", usd(1), strings.Repeat("a", 101), "", false, domain.ErrInvalidReference},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := domain.NewDraft(domain.MerchantID{1}, tt.method, tt.amount, tt.reference, domain.CardInput{Token: tt.card, ManualCapture: tt.manual})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewDraft() error = %v, want %v", err, tt.wantErr)
			}
			if err == nil && (d.Method() != domain.Method(tt.method) || d.Amount() != tt.amount || d.Reference() != tt.reference || d.Card().Token != tt.card) {
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

// A card captured separately keeps its quote as long as its authorization lasts.
func TestDraftQuoteValidity(t *testing.T) {
	usd := money.New(2000, money.USD)
	for _, tc := range []struct {
		method string
		card   domain.CardInput
		want   time.Duration
	}{
		{"pix", domain.CardInput{}, domain.PixQuoteValidity},
		{"card", domain.CardInput{Token: "tok_approved"}, domain.CardQuoteValidity},
		{"card", domain.CardInput{Token: "tok_approved", ManualCapture: true}, domain.CardAuthorizationValidity},
	} {
		d, err := domain.NewDraft(domain.MerchantID{1}, tc.method, usd, "o", tc.card)
		if err != nil {
			t.Fatal(err)
		}
		if got := d.QuoteValidity(); got != tc.want {
			t.Errorf("%s %+v: QuoteValidity() = %v, want %v", tc.method, tc.card, got, tc.want)
		}
	}
}
