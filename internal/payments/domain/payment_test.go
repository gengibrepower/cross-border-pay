package domain_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
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
		payer     domain.PayerInput
	}{
		{"pix", "pix", usd(2000), "order-123", "", false, nil, domain.PayerInput{}},
		{"100-character reference", "pix", usd(1), strings.Repeat("é", 100), "", false, nil, domain.PayerInput{}},
		// Whether a method is offered is the providers' call, not the draft's.
		{"boleto", "boleto", usd(2000), "o", "", false, nil, maria},
		{"boleto without a payer", "boleto", usd(2000), "o", "", false, domain.ErrPayerRequired, domain.PayerInput{}},
		{"boleto with a card", "boleto", usd(2000), "o", "tok_approved", false, domain.ErrCardNotAllowed, maria},
		{"pix with a payer", "pix", usd(2000), "o", "", false, domain.ErrPayerNotAllowed, maria},
		{"card with a payer", "card", usd(2000), "o", "tok_approved", false, domain.ErrPayerNotAllowed, maria},
		{"card", "card", usd(2000), "o", "tok_approved", false, nil, domain.PayerInput{}},
		{"card without a token", "card", usd(2000), "o", "", false, domain.ErrCardRequired, domain.PayerInput{}},
		{"card number as token", "card", usd(2000), "o", "4111111111111111", false, domain.ErrInvalidCardToken, domain.PayerInput{}},
		{"token with a space", "card", usd(2000), "o", "tok_a b", false, domain.ErrInvalidCardToken, domain.PayerInput{}},
		{"token too long", "card", usd(2000), "o", "tok_" + strings.Repeat("a", 61), false, domain.ErrInvalidCardToken, domain.PayerInput{}},
		{"bare prefix", "card", usd(2000), "o", "tok_", false, domain.ErrInvalidCardToken, domain.PayerInput{}},
		{"60-character token", "card", usd(2000), "o", "tok_" + strings.Repeat("a", 60), false, nil, domain.PayerInput{}},
		{"pix with a card", "pix", usd(2000), "o", "tok_approved", false, domain.ErrCardNotAllowed, domain.PayerInput{}},
		{"manual card", "card", usd(2000), "o", "tok_approved", true, nil, domain.PayerInput{}},
		{"manual capture on pix", "pix", usd(2000), "o", "", true, domain.ErrCardNotAllowed, domain.PayerInput{}},
		{"unknown method", "PIX", usd(2000), "o", "", false, domain.ErrUnknownMethod, domain.PayerInput{}},
		{"charged in BRL", "pix", money.New(2000, money.BRL), "o", "", false, domain.ErrUnsupportedCurrency, domain.PayerInput{}},
		{"zero", "pix", usd(0), "o", "", false, domain.ErrInvalidAmount, domain.PayerInput{}},
		{"negative", "pix", usd(-1), "o", "", false, domain.ErrInvalidAmount, domain.PayerInput{}},
		{"empty reference", "pix", usd(1), "", "", false, domain.ErrInvalidReference, domain.PayerInput{}},
		{"101-character reference", "pix", usd(1), strings.Repeat("a", 101), "", false, domain.ErrInvalidReference, domain.PayerInput{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := domain.NewDraft(domain.MerchantID{1}, tt.method, tt.amount, tt.reference, domain.CardInput{Token: tt.card, ManualCapture: tt.manual}, tt.payer)
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

var maria = domain.PayerInput{Name: "Maria Silva", Document: "529.982.247-25", Email: "maria@example.com"}

func TestNewPayer(t *testing.T) {
	p, err := domain.NewPayer(domain.PayerInput{Name: "  Maria Silva ", Document: "529.982.247-25"})
	if err != nil || p.Name != "Maria Silva" || p.Document.Digits() != "52998224725" || p.Email != "" {
		t.Fatalf("NewPayer = %+v, %v", p, err)
	}
	for name, tc := range map[string]struct {
		in   domain.PayerInput
		want error
	}{
		"no name":          {domain.PayerInput{Name: " ", Document: "52998224725"}, domain.ErrInvalidPayerName},
		"long name":        {domain.PayerInput{Name: strings.Repeat("á", 101), Document: "52998224725"}, domain.ErrInvalidPayerName},
		"no CPF":           {domain.PayerInput{Name: "Maria"}, domain.ErrInvalidPayerCPF},
		"wrong digit":      {domain.PayerInput{Name: "Maria", Document: "52998224726"}, domain.ErrInvalidPayerCPF},
		"CNPJ":             {domain.PayerInput{Name: "Maria", Document: "11222333000181"}, domain.ErrInvalidPayerCPF},
		"bad e-mail":       {domain.PayerInput{Name: "Maria", Document: "52998224725", Email: "maria"}, domain.ErrInvalidEmail},
		"e-mail with name": {domain.PayerInput{Name: "Maria", Document: "52998224725", Email: "Maria <m@example.com>"}, domain.ErrInvalidEmail},
		"long e-mail":      {domain.PayerInput{Name: "Maria", Document: "52998224725", Email: strings.Repeat("a", 250) + "@x.io"}, domain.ErrInvalidEmail},
	} {
		if _, err := domain.NewPayer(tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", name, err, tc.want)
		}
	}
	if got := fmt.Sprintf("%+v", p); strings.Contains(got, "52998224725") {
		t.Errorf("a payer prints its CPF whole: %s", got)
	}
}

// A card captured separately keeps its quote as long as its authorization lasts.
func TestDraftSchedule(t *testing.T) {
	cal, err := calendar.Brazil()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
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
		d, err := domain.NewDraft(domain.MerchantID{1}, tc.method, usd, "o", tc.card, domain.PayerInput{})
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Schedule(&cal, now); got != tc.want || !d.Boleto().Due.IsZero() {
			t.Errorf("%s %+v: Schedule() = %v, due %s; want %v and no due date", tc.method, tc.card, got, d.Boleto().Due, tc.want)
		}
	}
}

// Decision 44: a boleto is due three calendar days later and its quote lasts until the
// end of the first banking day on or after that, when the bank stops receiving it.
func TestBoletoSchedule(t *testing.T) {
	cal, err := calendar.Brazil()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, now, due, until string
	}{
		// Friday 13 November 2026, 09:00 in São Paulo: due Monday 16.
		{"weekday", "2026-11-13T12:00:00Z", "2026-11-16", "2026-11-17T03:00:00Z"},
		// Wednesday 11: due Saturday 14, received until Monday 16.
		{"due on a Saturday", "2026-11-11T12:00:00Z", "2026-11-14", "2026-11-17T03:00:00Z"},
		// Tuesday 17: due Friday 20, Consciência Negra, received until Monday 23.
		{"due on a holiday", "2026-11-17T12:00:00Z", "2026-11-20", "2026-11-24T03:00:00Z"},
		// 23:30 on Friday 13 in São Paulo is already Saturday in UTC: São Paulo counts.
		{"late at night", "2026-11-14T02:30:00Z", "2026-11-16", "2026-11-17T03:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, _ := time.Parse(time.RFC3339, tc.now)
			until, _ := time.Parse(time.RFC3339, tc.until)
			d, err := domain.NewDraft(domain.MerchantID{1}, "boleto", money.New(2000, money.USD), "o", domain.CardInput{}, maria)
			if err != nil {
				t.Fatal(err)
			}
			validity := d.Schedule(&cal, now)
			if got := d.Boleto().Due.String(); got != tc.due {
				t.Errorf("due = %s, want %s", got, tc.due)
			}
			if got := now.Add(validity); !got.Equal(until) {
				t.Errorf("quote until %s, want %s", got, until)
			}
			if d.Boleto().Payer.Document.Digits() != "52998224725" {
				t.Errorf("payer = %+v", d.Boleto().Payer)
			}
		})
	}
}
