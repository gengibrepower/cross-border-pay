package domain_test

import (
	"errors"
	"testing"
	"time"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

func gameStoreQuote(t *testing.T) fx.Quote {
	t.Helper()
	rate, err := fx.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		t.Fatal(err)
	}
	q, err := fx.NewQuote(money.New(2000, money.USD), rate, fx.DefaultPricing, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// Every status a payment can be in when an attempt of it succeeds, and what the money
// does: confirm a pending payment paid in full and on time, and otherwise be held.
func TestConfirmPayment(t *testing.T) {
	q := gameStoreQuote(t)
	quoted, onTime := q.PayerAmount(), t0.Add(time.Second)
	for _, tc := range []struct {
		name   string
		status domain.Status
		amount money.Money
		paidAt time.Time
		kind   domain.DivergenceKind
	}{
		{"in full, on time", domain.Pending, quoted, onTime, ""},
		{"a cent short", domain.Pending, money.New(quoted.Amount()-1, money.BRL), onTime, domain.AmountMismatch},
		{"a cent over", domain.Pending, money.New(quoted.Amount()+1, money.BRL), onTime, domain.AmountMismatch},
		{"another currency", domain.Pending, money.New(quoted.Amount(), money.USD), onTime, domain.AmountMismatch},
		{"late", domain.Pending, quoted, t0.Add(time.Minute), domain.LatePayment},
		{"short and late", domain.Pending, money.New(1, money.BRL), t0.Add(time.Hour), domain.AmountMismatch},
		{"after expiring", domain.Expired, quoted, onTime, domain.LatePayment},
		{"after a cancel", domain.Canceled, quoted, onTime, domain.LatePayment},
		{"already paid", domain.Paid, quoted, onTime, domain.SecondCharge},
		{"after a refund", domain.Refunded, quoted, onTime, domain.SecondCharge},
		{"after a chargeback", domain.ChargedBack, quoted, onTime, domain.SecondCharge},
	} {
		to, err := domain.ConfirmPayment(tc.status, &q, tc.amount, tc.paidAt)
		kind, held := domain.DivergenceOf(err)
		switch {
		case tc.kind == "" && (err != nil || to != domain.Paid):
			t.Errorf("%s: ConfirmPayment() = %s, %v; want paid", tc.name, to, err)
		case tc.kind != "" && (!held || kind != tc.kind || to != tc.status):
			t.Errorf("%s: ConfirmPayment() = %s, %v (%q); want %s held as %s", tc.name, to, err, kind, tc.status, tc.kind)
		}
	}
}

func TestDivergenceOfOtherErrors(t *testing.T) {
	for _, err := range []error{nil, errors.New("connection reset"), domain.ErrBRCodeMismatch, domain.ErrInvalidTransition} {
		if kind, ok := domain.DivergenceOf(err); ok {
			t.Errorf("DivergenceOf(%v) = %s, want none", err, kind)
		}
	}
	if kind, ok := domain.DivergenceOf(domain.ErrExternalIDMismatch); !ok || kind != domain.SecondCharge {
		t.Errorf("another charge for the attempt = %s, %v; want %s", kind, ok, domain.SecondCharge)
	}
}

func TestDivergencePosting(t *testing.T) {
	receivables, held := ledger.AccountID{1}, ledger.AccountID{2}
	tr, err := domain.DivergencePosting("divergence:pixsim:chg_1", receivables, held, money.New(10150, money.BRL))
	if err != nil {
		t.Fatal(err)
	}
	got := postings(tr)
	if tr.Reference() != "divergence:pixsim:chg_1" || len(got) != 2 ||
		got[receivables] != money.New(10150, money.BRL) || got[held] != money.New(-10150, money.BRL) {
		t.Errorf("posting %q %v; want 101.50 from the provider held on divergence", tr.Reference(), got)
	}
	if _, err := domain.DivergencePosting("r", receivables, held, money.New(0, money.BRL)); err == nil {
		t.Error("posted nothing as a divergence")
	}
	if got := domain.DivergenceReference("pixsim", "chg_1"); got != "divergence:pixsim:chg_1" {
		t.Errorf("DivergenceReference() = %q", got)
	}
}
