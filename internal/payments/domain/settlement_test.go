package domain_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"pgregory.net/rapid"

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
	var payable, paid, reserve ledger.AccountID
	payable[0], paid[0], reserve[0] = 1, 2, 3
	id := domain.SettlementID{7}
	usd := func(c int64) money.Money { return money.New(c, money.USD) }
	tx, err := domain.SettlementPosting(id, usd(1746), usd(194), payable, paid, reserve)
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
	if got[payable] != -1940 || got[paid] != 1746 || got[reserve] != 194 || len(got) != 3 {
		t.Errorf("entries = %v, want payable -1940, settlements paid +1746 and reserve +194", got)
	}
	// No hold leaves the reserve out: the ledger refuses a zero leg.
	tx, err = domain.SettlementPosting(id, usd(9), usd(0), payable, paid, reserve)
	if err != nil || len(tx.Entries()) != 2 {
		t.Errorf("SettlementPosting(9, 0) = %d entries, %v; want 2, nil", len(tx.Entries()), err)
	}
	for _, c := range [][2]int64{{0, 0}, {-1, 0}, {10, -1}} {
		if _, err := domain.SettlementPosting(id, usd(c[0]), usd(c[1]), payable, paid, reserve); !errors.Is(err, domain.ErrInvalidAmount) {
			t.Errorf("SettlementPosting(%d, %d) error = %v, want ErrInvalidAmount", c[0], c[1], err)
		}
	}
}

// Decision 26: a tenth held, rounded down so the cent goes out now; nothing held on
// nothing, and no overflow near the top of int64.
func TestReserveHold(t *testing.T) {
	p := policy(t)
	for _, c := range [][2]int64{
		{1940, 194},
		{134, 13},
		{9, 0},
		{10, 1},
		{19_999, 1999},
		{0, 0},
		{-500, 0},
		{math.MaxInt64, math.MaxInt64 / 10},
	} {
		if got := p.ReserveHold(c[0]); got != c[1] {
			t.Errorf("ReserveHold(%d) = %d, want %d", c[0], got, c[1])
		}
	}
	if got := p.ReserveUntil(date(2026, time.November, 2)); !got.Equal(date(2027, time.January, 31)) {
		t.Errorf("ReserveUntil(2026-11-02) = %v, want 2027-01-31", got)
	}
}

func TestReserveRelease(t *testing.T) {
	for _, c := range []struct{ held, due, balance, want int64 }{
		{held: 388, due: 194, balance: 388, want: 194}, // nothing used: the due hold whole
		{held: 388, due: 194, balance: 88, want: 0},    // 300 used: the oldest hold is gone
		{held: 194, due: 194, balance: 88, want: 88},   // the next one, what the debt left
		{held: 388, due: 388, balance: 0, want: 0},
		{held: 388, due: 0, balance: 388, want: 0},
	} {
		if got := domain.ReserveRelease(c.held, c.due, c.balance); got != c.want {
			t.Errorf("ReserveRelease(held %d, due %d, balance %d) = %d, want %d", c.held, c.due, c.balance, got, c.want)
		}
	}
}

// Holds released one cutoff at a time give back exactly what the debt left, oldest
// holds used first, and never more than the reserve holds.
func TestReserveReleasesAddUpToWhatIsLeft(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		holds := rapid.SliceOfN(rapid.Int64Range(0, 10_000), 1, 8).Draw(t, "holds")
		var held int64
		for _, h := range holds {
			held += h
		}
		used := rapid.Int64Range(0, held).Draw(t, "used")
		balance := held - used
		var released int64
		for _, h := range holds {
			r := domain.ReserveRelease(held, h, balance)
			if r < 0 || r > h || r > balance {
				t.Fatalf("release %d of a hold of %d with %d in the reserve", r, h, balance)
			}
			released += r
			held -= h
			balance -= r
		}
		if balance != 0 || released != sum(holds)-used {
			t.Fatalf("released %d of holds %v with %d used, want %d", released, holds, used, sum(holds)-used)
		}
	})
}

func sum(xs []int64) int64 {
	var s int64
	for _, x := range xs {
		s += x
	}
	return s
}

func TestReserveCover(t *testing.T) {
	for _, c := range [][3]int64{{-300, 388, 300}, {-2000, 194, 194}, {0, 100, 0}, {50, 100, 0}, {-10, 0, 0}} {
		if got := domain.ReserveCover(c[0], c[1]); got != c[2] {
			t.Errorf("ReserveCover(%d, %d) = %d, want %d", c[0], c[1], got, c[2])
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

// Decision 27: owing exactly US$ 1,000.00 still takes payments; a cent more does not.
func TestOverDebtLimit(t *testing.T) {
	for position, want := range map[int64]bool{0: false, 500: false, -100_000: false, -100_001: true, math.MinInt64: true} {
		if got := domain.OverDebtLimit(position); got != want {
			t.Errorf("OverDebtLimit(%d) = %v, want %v", position, got, want)
		}
	}
}
