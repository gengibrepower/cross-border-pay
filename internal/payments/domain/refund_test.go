package domain_test

import (
	"errors"
	"maps"
	"testing"
	"time"

	"pgregory.net/rapid"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

var refundAccounts = domain.RefundAccounts{
	ProviderReceivables: ledger.AccountID{1},
	FXPayer:             ledger.AccountID{2},
	FXMerchant:          ledger.AccountID{3},
	MerchantPayable:     ledger.AccountID{4},
	RefundsInFlight:     ledger.AccountID{5},
	SpreadRevenue:       ledger.AccountID{6},
	FXResult:            ledger.AccountID{7},
}

func usd(n int64) money.Money { return money.New(n, money.USD) }

func brl(n int64) money.Money { return money.New(n, money.BRL) }

func rate(t testing.TB, s string) fx.Rate {
	t.Helper()
	r, err := fx.ParseRate(money.USD, money.BRL, s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func gameStore(t testing.TB) fx.Quote {
	t.Helper()
	q, err := fx.NewQuote(usd(2000), rate(t, "5.0000"), fx.DefaultPricing, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// The worked example of the F7 plan: US$ 10.00 of the GameStore's US$ 20.00.
func TestRefundGameStore(t *testing.T) {
	q := gameStore(t)
	none := domain.NoRefunds(&q)
	r, err := domain.PriceRefund(&q, &none, usd(1000))
	if err != nil {
		t.Fatal(err)
	}
	if want := (domain.RefundAmounts{Amount: usd(1000), PayerAmount: brl(5075), Spread: usd(15)}); r != want {
		t.Fatalf("PriceRefund() = %+v, want %+v", r, want)
	}
	id := domain.RefundID{7}
	a := &refundAccounts

	hold, err := domain.RefundHoldPosting(id, &r, a)
	if err != nil {
		t.Fatal(err)
	}
	checkPostings(t, hold, domain.RefundReference(id, domain.RefundHold), map[ledger.AccountID]money.Money{
		a.MerchantPayable: usd(-1000),
		a.RefundsInFlight: usd(1000),
	})

	done, err := domain.RefundCompletionPosting(id, &r, rate(t, "5.0000"), a)
	if err != nil {
		t.Fatal(err)
	}
	// 5075 / 5.0000 = 1015 = 1000 + 15: the exchange result is zero and left out.
	checkPostings(t, done, domain.RefundReference(id, domain.RefundComplete), map[ledger.AccountID]money.Money{
		a.ProviderReceivables: brl(-5075),
		a.FXPayer:             brl(5075),
		a.FXMerchant:          usd(1015),
		a.RefundsInFlight:     usd(-1000),
		a.SpreadRevenue:       usd(-15),
	})

	release, err := domain.RefundReleasePosting(id, &r, a)
	if err != nil {
		t.Fatal(err)
	}
	checkPostings(t, release, domain.RefundReference(id, domain.RefundRelease), map[ledger.AccountID]money.Money{
		a.RefundsInFlight: usd(-1000),
		a.MerchantPayable: usd(1000),
	})
}

// Decision 13: the payer gets back the same reais whatever the rate does; the gateway
// absorbs the difference in dollars.
func TestRefundExchangeResult(t *testing.T) {
	q := gameStore(t)
	none := domain.NoRefunds(&q)
	r, err := domain.PriceRefund(&q, &none, usd(1000))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rate         string
		cost, result int64
	}{
		// 5075 / 5.5 = 922.72… → 923; 1000 + 15 - 923 = 92 gained.
		{"5.5000", 923, 92},
		// 5075 / 4.5 = 1127.77… → 1128; 1000 + 15 - 1128 = -113 lost.
		{"4.5000", 1128, -113},
	} {
		done, err := domain.RefundCompletionPosting(domain.RefundID{1}, &r, rate(t, tc.rate), &refundAccounts)
		if err != nil {
			t.Fatal(err)
		}
		got := postings(done)
		if got[refundAccounts.FXMerchant] != usd(tc.cost) || got[refundAccounts.FXResult] != usd(tc.result) ||
			got[refundAccounts.ProviderReceivables] != brl(-5075) {
			t.Errorf("at %s: %v; want cost %d, result %d and R$ 50.75 to the payer", tc.rate, got, tc.cost, tc.result)
		}
	}
}

// Rounding each half on its own would return R$ 50.76 twice, R$ 101.52 of R$ 101.51.
func TestRefundSharesAreCumulative(t *testing.T) {
	// US$ 20.00 at 5.0005 with 1.5% spread: 10151.015 → R$ 101.51.
	q, err := fx.NewQuote(usd(2000), rate(t, "5.0005"), fx.DefaultPricing, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if q.PayerAmount().Amount()%2 != 1 {
		t.Fatalf("payer amount %s is even; the test needs an odd one", q.PayerAmount())
	}
	before := domain.NoRefunds(&q)
	var halves []int64
	for range 2 {
		r, err := domain.PriceRefund(&q, &before, usd(1000))
		if err != nil {
			t.Fatal(err)
		}
		halves = append(halves, r.PayerAmount.Amount())
		if before, err = before.Add(&r); err != nil {
			t.Fatal(err)
		}
	}
	if before.PayerAmount != q.PayerAmount() || before.Spread != q.Spread() || halves[0] == halves[1] {
		t.Errorf("halves %v, %+v in all; want two different halves adding up to %s and %s", halves, before, q.PayerAmount(), q.Spread())
	}
}

func TestRefundRefusals(t *testing.T) {
	q := gameStore(t)
	none := domain.NoRefunds(&q)
	partial, err := domain.PriceRefund(&q, &none, usd(1500))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		before domain.RefundAmounts
		amount money.Money
		want   error
	}{
		{"more than paid", none, usd(2001), domain.ErrRefundExceedsPayment},
		{"more than is left", partial, usd(501), domain.ErrRefundExceedsPayment},
		{"zero", none, usd(0), domain.ErrInvalidAmount},
		{"negative", none, usd(-1), domain.ErrInvalidAmount},
		{"another currency", none, brl(100), money.ErrCurrencyMismatch},
	} {
		if _, err := domain.PriceRefund(&q, &tc.before, tc.amount); !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", tc.name, err, tc.want)
		}
	}
	// 75% of R$ 101.50 is 7612.5, half-even 7612: the last quarter gets the 2538 left.
	if partial.PayerAmount != brl(7612) {
		t.Fatalf("three quarters = %s, want R$ 76.12", partial.PayerAmount)
	}
	if r, err := domain.PriceRefund(&q, &partial, usd(500)); err != nil || r.PayerAmount != brl(2538) {
		t.Errorf("refund of what is left = %+v, %v; want the R$ 25.38 left", r, err)
	}
}

// A refund whose share of reais rounds to nothing returns nothing: refused, not posted.
func TestRefundTooSmall(t *testing.T) {
	// A payment of US$ 1,000.00 at 0.0010 BRL/USD: the payer paid R$ 1.02, so one cent
	// of the dollars is a thousandth of a centavo.
	q, err := fx.NewQuote(usd(100_000), rate(t, "0.0010"), fx.DefaultPricing, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	none := domain.NoRefunds(&q)
	if _, err := domain.PriceRefund(&q, &none, usd(1)); !errors.Is(err, domain.ErrRefundTooSmall) {
		t.Errorf("error = %v, want %v", err, domain.ErrRefundTooSmall)
	}
}

// Invariant 8 in the domain: whatever refunds are made, and whichever of them the
// provider later refuses, what counts never exceeds what was paid, every posting
// balances, and refunding what is left returns exactly the reais and the spread that
// are still owed.
func TestRefundsOfAnyPaymentAddUp(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r, err := fx.NewRate(money.USD, money.BRL, rapid.Int64Range(1_000_000, 100_000_000_000).Draw(t, "rate"))
		if err != nil {
			t.Fatal(err)
		}
		pricing := fx.Pricing{
			Spread: fx.BasisPoints(rapid.Int64Range(0, 2_000).Draw(t, "spread")),
			Fee:    fx.BasisPoints(rapid.Int64Range(0, 2_000).Draw(t, "fee")),
		}
		q, err := fx.NewQuote(usd(rapid.Int64Range(1, 1_000_000_000).Draw(t, "amount")), r, pricing, t0, t0.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		today, err := fx.NewRate(money.USD, money.BRL, rapid.Int64Range(1_000_000, 100_000_000_000).Draw(t, "today"))
		if err != nil {
			t.Fatal(err)
		}

		counted := domain.NoRefunds(&q)
		var pending []domain.RefundAmounts
		for i := range rapid.IntRange(0, 12).Draw(t, "steps") {
			if len(pending) > 0 && rapid.Bool().Draw(t, "reject") {
				k := rapid.IntRange(0, len(pending)-1).Draw(t, "which")
				counted = subtract(t, &counted, &pending[k])
				pending = append(pending[:k], pending[k+1:]...)
				continue
			}
			amount := usd(rapid.Int64Range(1, q.Amount().Amount()).Draw(t, "refund"))
			refund, err := domain.PriceRefund(&q, &counted, amount)
			over := counted.Amount.Amount()+amount.Amount() > q.Amount().Amount()
			switch {
			case over:
				if !errors.Is(err, domain.ErrRefundExceedsPayment) {
					t.Fatalf("refund %d of %s over %s counted: error = %v, want exceeds", i, amount, counted.Amount, err)
				}
				continue
			case errors.Is(err, domain.ErrRefundTooSmall):
				continue
			case err != nil:
				t.Fatalf("refund %d of %s: %v", i, amount, err)
			}
			checkRefund(t, &refund, today)
			if counted, err = counted.Add(&refund); err != nil {
				t.Fatal(err)
			}
			pending = append(pending, refund)
			if counted.PayerAmount.Amount() > q.PayerAmount().Amount() {
				t.Fatalf("%s counted to the payer, who paid %s", counted.PayerAmount, q.PayerAmount())
			}
		}

		left, err := q.Amount().Sub(counted.Amount)
		if err != nil || left.IsZero() {
			return
		}
		last, err := domain.PriceRefund(&q, &counted, left)
		if errors.Is(err, domain.ErrRefundTooSmall) {
			if counted.PayerAmount != q.PayerAmount() {
				t.Fatalf("the rest refused as too small with %s of %s returned", counted.PayerAmount, q.PayerAmount())
			}
			return
		}
		if err != nil {
			t.Fatalf("refund of the %s left: %v", left, err)
		}
		checkRefund(t, &last, today)
		if counted, err = counted.Add(&last); err != nil {
			t.Fatal(err)
		}
		if counted.Amount != q.Amount() || counted.PayerAmount != q.PayerAmount() || counted.Spread != q.Spread() {
			t.Fatalf("fully refunded %+v, want exactly %s, %s and %s", counted, q.Amount(), q.PayerAmount(), q.Spread())
		}
	})
}

func subtract(t *rapid.T, total, r *domain.RefundAmounts) domain.RefundAmounts {
	neg := func(m money.Money) money.Money {
		n, err := m.Neg()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	out, err := total.Add(&domain.RefundAmounts{Amount: neg(r.Amount), PayerAmount: neg(r.PayerAmount), Spread: neg(r.Spread)})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// checkRefund posts every step of the refund: each must balance per currency and give
// the payer exactly the refund's reais.
func checkRefund(t *rapid.T, r *domain.RefundAmounts, today fx.Rate) {
	if r.PayerAmount.Amount() <= 0 {
		t.Fatalf("refund %+v returns nothing to the payer", r)
	}
	id := domain.RefundID{1}
	hold, err := domain.RefundHoldPosting(id, r, &refundAccounts)
	if err != nil {
		t.Fatalf("hold %+v: %v", r, err)
	}
	release, err := domain.RefundReleasePosting(id, r, &refundAccounts)
	if err != nil {
		t.Fatalf("release %+v: %v", r, err)
	}
	done, err := domain.RefundCompletionPosting(id, r, today, &refundAccounts)
	if err != nil {
		t.Fatalf("complete %+v at %s: %v", r, today, err)
	}
	for _, tx := range []ledger.Transaction{hold, release, done} {
		sums := map[money.Currency]int64{}
		for _, e := range tx.Entries() {
			sums[e.Amount().Currency()] += e.Amount().Amount()
		}
		for c, s := range sums {
			if s != 0 {
				t.Fatalf("%s: %s sums to %d", tx.Reference(), c.Code(), s)
			}
		}
	}
	if got := postings(done)[refundAccounts.ProviderReceivables]; got.Amount() != -r.PayerAmount.Amount() {
		t.Fatalf("completion takes %s from the provider, want %s", got, r.PayerAmount)
	}
	if got := postings(hold)[refundAccounts.MerchantPayable]; got.Amount() != -r.Amount.Amount() {
		t.Fatalf("hold takes %s from the merchant, want %s", got, r.Amount)
	}
}

func checkPostings(t *testing.T, tx ledger.Transaction, reference string, want map[ledger.AccountID]money.Money) {
	t.Helper()
	if got := postings(tx); !maps.Equal(got, want) || tx.Reference() != reference {
		t.Errorf("%s = %v, want %s = %v", tx.Reference(), got, reference, want)
	}
}
