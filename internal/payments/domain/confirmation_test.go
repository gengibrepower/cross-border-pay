package domain_test

import (
	"maps"
	"testing"
	"time"

	"pgregory.net/rapid"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

var accounts = domain.ConfirmationAccounts{
	ProviderReceivables: ledger.AccountID{1},
	FXPayer:             ledger.AccountID{2},
	FXMerchant:          ledger.AccountID{3},
	MerchantPayable:     ledger.AccountID{4},
	FeeRevenue:          ledger.AccountID{5},
	SpreadRevenue:       ledger.AccountID{6},
	Rounding:            ledger.AccountID{7},
}

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func postings(t ledger.Transaction) map[ledger.AccountID]money.Money {
	m := make(map[ledger.AccountID]money.Money)
	for _, e := range t.Entries() {
		m[e.Account()] = e.Amount()
	}
	return m
}

// Section 3.3, entry for entry; the rounding leg is zero here and left out.
func TestConfirmationGameStore(t *testing.T) {
	rate, err := fx.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		t.Fatal(err)
	}
	q, err := fx.NewQuote(money.New(2000, money.USD), rate, fx.DefaultPricing, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	id := domain.PaymentID{9}
	tx, err := domain.Confirmation(id, &q, &accounts)
	if err != nil {
		t.Fatal(err)
	}
	want := map[ledger.AccountID]money.Money{
		accounts.ProviderReceivables: money.New(10150, money.BRL),
		accounts.FXPayer:             money.New(-10150, money.BRL),
		accounts.FXMerchant:          money.New(-2030, money.USD),
		accounts.MerchantPayable:     money.New(1940, money.USD),
		accounts.FeeRevenue:          money.New(60, money.USD),
		accounts.SpreadRevenue:       money.New(30, money.USD),
	}
	if got := postings(tx); !maps.Equal(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
	if tx.Reference() != domain.ConfirmationReference(id) {
		t.Errorf("reference = %q, want %q", tx.Reference(), domain.ConfirmationReference(id))
	}
}

// Any quote confirms into a balanced transaction with no zero leg, crediting the
// merchant exactly its net and the provider exactly what the payer paid.
func TestConfirmationOfAnyQuoteBalances(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		rate, err := fx.NewRate(money.USD, money.BRL, rapid.Int64Range(1_000_000, 100_000_000_000).Draw(t, "rate"))
		if err != nil {
			t.Fatal(err)
		}
		pricing := fx.Pricing{
			Spread: fx.BasisPoints(rapid.Int64Range(0, 2_000).Draw(t, "spread")),
			Fee:    fx.BasisPoints(rapid.Int64Range(0, 2_000).Draw(t, "fee")),
		}
		amount := money.New(rapid.Int64Range(1, 1_000_000_000_000).Draw(t, "amount"), money.USD)
		q, err := fx.NewQuote(amount, rate, pricing, t0, t0.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}

		tx, err := domain.Confirmation(domain.PaymentID{1}, &q, &accounts)
		if err != nil {
			t.Fatalf("quote %+v: %v", q, err)
		}
		sums := map[money.Currency]int64{}
		for _, e := range tx.Entries() {
			if e.Amount().IsZero() {
				t.Fatalf("zero leg on %s", e.Account())
			}
			sums[e.Amount().Currency()] += e.Amount().Amount()
		}
		for c, s := range sums {
			if s != 0 {
				t.Fatalf("%s sums to %d", c.Code(), s)
			}
		}
		got := postings(tx)
		for account, want := range map[ledger.AccountID]money.Money{
			accounts.MerchantPayable:     q.MerchantNet(),
			accounts.ProviderReceivables: q.PayerAmount(),
		} {
			if posted, ok := got[account]; ok == want.IsZero() || (ok && posted != want) {
				t.Fatalf("%s got %v (posted %v), want %v", account, posted, ok, want)
			}
		}
	})
}
