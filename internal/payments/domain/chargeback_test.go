package domain_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"pgregory.net/rapid"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

var chargebackAccounts = domain.ChargebackAccounts{
	ProviderReceivables: ledger.AccountID{1},
	FXPayer:             ledger.AccountID{2},
	FXMerchant:          ledger.AccountID{3},
	MerchantPayable:     ledger.AccountID{4},
	SpreadRevenue:       ledger.AccountID{6},
	FXResult:            ledger.AccountID{7},
}

func entriesOf(tx ledger.Transaction) map[ledger.AccountID]int64 {
	got := map[ledger.AccountID]int64{}
	for _, e := range tx.Entries() {
		got[e.Account()] += e.Amount().Amount()
	}
	return got
}

// The GameStore charged back whole: the R$ 101.50 leave the acquirer, the merchant
// loses the US$ 20.00, fee kept, and the spread its US$ 0.30. At today's 5.5000 the
// reais cost US$ 18.45 to buy back, so the gateway gains US$ 1.85; the reversal of a
// won dispute at 4.5000 sells them for US$ 22.56 and loses US$ 2.26.
func TestChargebackGameStore(t *testing.T) {
	q := gameStore(t)
	none := domain.NoChargebacks(&q)
	c, err := domain.PriceChargeback(&q, &none, brl(10150))
	if err != nil {
		t.Fatal(err)
	}
	if c.PayerAmount != brl(10150) || c.Amount != usd(2000) || c.Spread != q.Spread() {
		t.Fatalf("PriceChargeback(whole) = %+v, want R$ 101.50, US$ 20.00 and the spread %s", c, q.Spread())
	}
	id := domain.ChargebackID{9}
	debit, err := domain.ChargebackDebitPosting(id, &c, rate(t, "5.5000"), &chargebackAccounts)
	if err != nil {
		t.Fatal(err)
	}
	a := chargebackAccounts
	got := entriesOf(debit)
	if debit.Reference() != "chargeback:"+id.String()+":debit" || got[a.ProviderReceivables] != -10150 || got[a.FXPayer] != 10150 ||
		got[a.MerchantPayable] != -2000 || got[a.SpreadRevenue] != -q.Spread().Amount() || got[a.FXMerchant] != 1845 ||
		got[a.FXResult] != 2000+q.Spread().Amount()-1845 {
		t.Errorf("debit = %v", got)
	}
	back, err := domain.ChargebackReversalPosting(id, &c, rate(t, "4.5000"), &chargebackAccounts)
	if err != nil {
		t.Fatal(err)
	}
	got = entriesOf(back)
	if back.Reference() != "chargeback:"+id.String()+":reversal" || got[a.ProviderReceivables] != 10150 || got[a.MerchantPayable] != 2000 ||
		got[a.SpreadRevenue] != q.Spread().Amount() || got[a.FXMerchant] != -2256 || got[a.FXResult] != -(2000+q.Spread().Amount()-2256) {
		t.Errorf("reversal = %v", got)
	}
}

func TestChargebackRefusals(t *testing.T) {
	q := gameStore(t)
	none := domain.NoChargebacks(&q)
	half, err := domain.PriceChargeback(&q, &none, brl(5075))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		before *domain.ChargebackAmounts
		payer  money.Money
		want   error
	}{
		"more than paid":    {&none, brl(10151), domain.ErrChargebackExceedsPayment},
		"more than is left": {&half, brl(5076), domain.ErrChargebackExceedsPayment},
		"zero":              {&none, brl(0), domain.ErrInvalidAmount},
		"in dollars":        {&none, usd(100), money.ErrCurrencyMismatch},
	} {
		if _, err := domain.PriceChargeback(&q, c.before, c.payer); !errors.Is(err, c.want) {
			t.Errorf("%s: error = %v, want %v", name, err, c.want)
		}
	}
}

// Chargebacks of any payment, in any pieces, add up to it exactly: the merchant loses
// the whole amount and the spread returns whole, whatever each piece rounded to, and
// every posting balances at any rate.
func TestChargebacksOfAnyPaymentAddUp(t *testing.T) {
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
		if q.PayerAmount().Amount() == 0 {
			t.Skip("the payer paid nothing: there is nothing to take back")
		}
		total := domain.NoChargebacks(&q)
		for total.PayerAmount != q.PayerAmount() {
			left := q.PayerAmount().Amount() - total.PayerAmount.Amount()
			piece := rapid.Int64Range(1, left).Draw(t, "piece")
			c, err := domain.PriceChargeback(&q, &total, brl(piece))
			if err != nil {
				t.Fatalf("chargeback of %d with %d left: %v", piece, left, err)
			}
			if c.Amount.IsNegative() {
				t.Fatalf("chargeback %+v gives the merchant money", c)
			}
			for _, post := range []func(domain.ChargebackID, *domain.ChargebackAmounts, fx.Rate, *domain.ChargebackAccounts) (ledger.Transaction, error){
				domain.ChargebackDebitPosting, domain.ChargebackReversalPosting,
			} {
				if _, err := post(domain.ChargebackID{1}, &c, today, &chargebackAccounts); err != nil {
					t.Fatalf("posting %+v at %s: %v", c, today, err)
				}
			}
			if total, err = total.Add(&c); err != nil {
				t.Fatal(err)
			}
		}
		if total.Amount != q.Amount() || total.Spread != q.Spread() {
			t.Fatalf("chargebacks of the whole payer amount = %+v, want %s and %s", total, q.Amount(), q.Spread())
		}
	})
}

// The provider decides the dispute; the gateway takes its word while the dispute moves
// forward, ignores an older answer and refuses a contradiction. Every pair is checked
// against a table written again here.
func TestMoveChargeback(t *testing.T) {
	type pair struct{ from, to domain.ChargebackStatus }
	moves := map[pair]bool{
		{domain.NeedsResponse, domain.UnderReview}: true,
		{domain.NeedsResponse, domain.Lost}:        true,
		{domain.UnderReview, domain.Won}:           true,
		{domain.UnderReview, domain.Lost}:          true,
	}
	refused := map[pair]bool{
		{domain.NeedsResponse, domain.Won}:  true,
		{domain.Won, domain.Lost}:           true,
		{domain.Lost, domain.Won}:           true,
		{domain.Won, domain.UnderReview}:    true,
		{domain.Won, domain.NeedsResponse}:  true,
		{domain.Lost, domain.UnderReview}:   true,
		{domain.Lost, domain.NeedsResponse}: true,
	}
	all := []domain.ChargebackStatus{domain.NeedsResponse, domain.UnderReview, domain.Won, domain.Lost}
	for _, from := range all {
		for _, to := range all {
			got, changed, err := domain.MoveChargeback(from, to)
			p := pair{from, to}
			switch {
			case moves[p]:
				if err != nil || !changed || got != to {
					t.Errorf("%s → %s = %s, %v, %v; want %s, moved", from, to, got, changed, err, to)
				}
			case refused[p]:
				if !errors.Is(err, domain.ErrChargebackClosed) || changed || got != from {
					t.Errorf("%s → %s = %s, %v, %v; want ErrChargebackClosed", from, to, got, changed, err)
				}
			default: // the same status, or an older answer
				if err != nil || changed || got != from {
					t.Errorf("%s → %s = %s, %v, %v; want it unchanged", from, to, got, changed, err)
				}
			}
		}
	}
	if _, _, err := domain.MoveChargeback(domain.NeedsResponse, "disputed"); !errors.Is(err, domain.ErrUnknownChargeback) {
		t.Errorf("unknown status: error = %v", err)
	}
}

func TestChargebackTexts(t *testing.T) {
	if r, err := domain.NewReason("  fraudulent "); err != nil || r != "fraudulent" {
		t.Errorf("NewReason = %q, %v", r, err)
	}
	long := string(make([]rune, 101))
	for _, s := range []string{"", "   ", long} {
		if _, err := domain.NewReason(s); !errors.Is(err, domain.ErrInvalidReason) {
			t.Errorf("NewReason(%d runes) error = %v", len([]rune(s)), err)
		}
	}
	if _, err := domain.NewEvidence(string(make([]rune, 2000))); err != nil {
		t.Errorf("NewEvidence(2000) error = %v", err)
	}
	if _, err := domain.NewEvidence(string(make([]rune, 2001))); !errors.Is(err, domain.ErrInvalidEvidence) {
		t.Errorf("NewEvidence(2001) error = %v", err)
	}
	id, err := domain.ParseChargebackID(domain.ChargebackID{3}.String())
	if err != nil || id != (domain.ChargebackID{3}) {
		t.Errorf("ParseChargebackID round trip = %v, %v", id, err)
	}
	if _, err := domain.ParseChargebackID(domain.PaymentID{3}.String()); !errors.Is(err, domain.ErrInvalidChargebackID) {
		t.Errorf("ParseChargebackID(pay_) error = %v", err)
	}
}

func TestChargebackEventPayload(t *testing.T) {
	c := domain.ChargebackCase{
		ID: domain.ChargebackID{1}, Status: domain.Won, Reason: "fraudulent",
		Amounts: domain.ChargebackAmounts{PayerAmount: brl(10150), Amount: usd(2000), Spread: usd(30)},
	}
	e, err := domain.NewChargebackEvent(domain.EventID{2}, domain.ChargebackWonEvent, &c,
		&domain.PaymentSnapshot{ID: domain.PaymentID{3}, Merchant: domain.MerchantID{4}, Version: 5, Status: domain.ChargedBack}, t0)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type           string `json:"type"`
		PaymentVersion int    `json:"payment_version"`
		Data           struct {
			ChargebackID  string `json:"chargeback_id"`
			Status        string `json:"status"`
			Amount        struct{ Value int64 }
			PaymentStatus string `json:"payment_status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(e.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "chargeback.won" || got.PaymentVersion != 5 || got.Data.ChargebackID != c.ID.String() ||
		got.Data.Status != "won" || got.Data.Amount.Value != 2000 || got.Data.PaymentStatus != "charged_back" || e.Merchant != (domain.MerchantID{4}) {
		t.Errorf("event = %+v from %s", got, e.Payload)
	}
}
