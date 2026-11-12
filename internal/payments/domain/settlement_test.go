package domain_test

import (
	"errors"
	"testing"
	"time"

	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

func policy(t *testing.T) domain.SettlementPolicy {
	t.Helper()
	p, err := domain.DefaultSettlementPolicy()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// Decision 20: the cutoff is the local date in São Paulo (UTC-3), not the UTC one.
func TestCutoffDateIsLocal(t *testing.T) {
	p := policy(t)
	for _, tc := range []struct {
		now  time.Time
		want time.Time
	}{
		{time.Date(2026, 11, 2, 2, 59, 59, 0, time.UTC), date(2026, 11, 1)},
		{time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC), date(2026, 11, 2)},
		{time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), date(2026, 11, 2)},
	} {
		if got := p.CutoffDate(tc.now); !got.Equal(tc.want) {
			t.Errorf("CutoffDate(%v) = %v, want %v", tc.now, got, tc.want)
		}
	}
}

// Decision 25: PIX paid on local day P is available from P+1, boleto from P+2, card
// from P+30, so by cutoff D a payment counts when it was paid before the local
// midnight that starts D-days+1.
func TestAvailableBefore(t *testing.T) {
	p := policy(t)
	cutoff := date(2026, 11, 10)
	for _, tc := range []struct {
		method domain.Method
		want   time.Time
	}{
		{domain.Pix, time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)},
		{domain.Boleto, time.Date(2026, 11, 9, 3, 0, 0, 0, time.UTC)},
		{domain.Card, time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)},
	} {
		if got := p.AvailableBefore(tc.method, cutoff); !got.Equal(tc.want) {
			t.Errorf("AvailableBefore(%s, %v) = %v, want %v", tc.method, cutoff, got, tc.want)
		}
	}

	// The edges of a PIX paid on the evening of the 9th, local time: 23:59 local is
	// 02:59 UTC on the 10th, still the 9th, so available on the 10th; a minute later it
	// is the 10th and waits for the 11th.
	before := p.AvailableBefore(domain.Pix, cutoff)
	if late := time.Date(2026, 11, 10, 2, 59, 0, 0, time.UTC); !late.Before(before) {
		t.Errorf("PIX at 23:59 local on the 9th not available on the 10th")
	}
	if next := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC); next.Before(before) {
		t.Errorf("PIX at 00:00 local on the 10th available on the 10th")
	}
}

func TestSettlementPosting(t *testing.T) {
	var payable, paid ledger.AccountID
	payable[0], paid[0] = 1, 2
	id := domain.SettlementID{7}
	tx, err := domain.SettlementPosting(id, money.New(1940, money.USD), payable, paid)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Reference() != domain.SettlementReference(id) {
		t.Errorf("reference = %q, want %q", tx.Reference(), domain.SettlementReference(id))
	}
	got := map[ledger.AccountID]int64{}
	for _, e := range tx.Entries() {
		got[e.Account()] += e.Amount().Amount()
	}
	if got[payable] != -1940 || got[paid] != 1940 || len(got) != 2 {
		t.Errorf("entries = %v, want payable -1940 and settlements paid +1940", got)
	}
	for _, amount := range []int64{0, -1} {
		if _, err := domain.SettlementPosting(id, money.New(amount, money.USD), payable, paid); !errors.Is(err, domain.ErrInvalidAmount) {
			t.Errorf("SettlementPosting(%d) error = %v, want ErrInvalidAmount", amount, err)
		}
	}
}

func TestSettlementIDRoundTrip(t *testing.T) {
	id := domain.SettlementID{1, 2, 3}
	got, err := domain.ParseSettlementID(id.String())
	if err != nil || got != id {
		t.Errorf("ParseSettlementID(%q) = %v, %v; want %v", id.String(), got, err, id)
	}
	if _, err := domain.ParseSettlementID("pay_" + id.String()[4:]); !errors.Is(err, domain.ErrInvalidSettlementID) {
		t.Errorf("a payment ID parsed as a settlement ID: %v", err)
	}
}
