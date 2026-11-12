package main

import (
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

// settler is the worker's settlement job against the contract's database and clock.
func (c *contract) settler() *paymentsapp.Settler {
	c.t.Helper()
	policy, err := payments.DefaultSettlementPolicy()
	if err != nil {
		c.t.Fatal(err)
	}
	return paymentsapp.NewSettler(paymentsadapters.NewPostgres(c.pool, paymentSinks()), policy, c.clock, slog.New(slog.DiscardHandler))
}

func (c *contract) settle(s *paymentsapp.Settler) int {
	c.t.Helper()
	n, err := s.Settle(c.t.Context())
	if err != nil {
		c.t.Fatalf("Settle() error = %v", err)
	}
	return n
}

// toLocalDay moves the clock to 00:00:30 in São Paulo on November day, half a minute
// after that day's cutoff opens. t0 is 09:00 local on November 1.
func (c *contract) toLocalDay(day int) {
	c.t.Helper()
	target := time.Date(2026, 11, day, 3, 0, 30, 0, time.UTC)
	if !target.After(c.clock.Now()) {
		c.t.Fatalf("clock at %v is already past %v", c.clock.Now(), target)
	}
	c.clock.Advance(target.Sub(c.clock.Now()))
}

type storedSettlement struct {
	id       pgtype.UUID
	cutoff   time.Time
	amount   int64
	payments []string
}

func (c *contract) settlements() []storedSettlement {
	c.t.Helper()
	rows, err := c.pool.Query(c.t.Context(), "SELECT id, cutoff_date, amount FROM settlements ORDER BY cutoff_date")
	if err != nil {
		c.t.Fatal(err)
	}
	var out []storedSettlement
	for rows.Next() {
		var s storedSettlement
		if err := rows.Scan(&s.id, &s.cutoff, &s.amount); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		c.t.Fatal(err)
	}
	for i := range out {
		out[i].payments = c.paymentsOf(out[i].id)
	}
	return out
}

func (c *contract) paymentsOf(settlement pgtype.UUID) []string {
	c.t.Helper()
	rows, err := c.pool.Query(c.t.Context(), "SELECT id FROM payments WHERE settlement_id = $1", settlement)
	if err != nil {
		c.t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			c.t.Fatal(err)
		}
		ids = append(ids, payments.PaymentID(id.Bytes).String())
	}
	if err := rows.Err(); err != nil {
		c.t.Fatal(err)
	}
	slices.Sort(ids)
	return ids
}

func sorted(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

func (c *contract) refund(key, paymentID string, cents int64) {
	c.t.Helper()
	r := refundOf(c.t, c.do(postRefund(c.t, key, "/v1/payments/"+paymentID, fmt.Sprintf("r-%s-%d-%d", paymentID, cents, c.count("refunds")), usdBody(cents)), true))
	if r.Status != "succeeded" {
		c.t.Fatalf("refund of %d on %s = %+v, want succeeded", cents, paymentID, r)
	}
}

// F7's done criterion: the settlement run twice for the same cutoff pays once. The
// GameStore's PIX paid on the 1st is pending that day, available on the 2nd, and its
// US$ 19.40 leave the payable for settlements paid.
func TestSettlementPaysWhatIsAvailableOnce(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	s := c.settler()

	if n := c.settle(s); n != 0 {
		t.Fatalf("settled %d merchants on the day of the payment, want 0: PIX is D+1", n)
	}
	c.clock.Advance(14*time.Hour + 59*time.Minute) // 23:59 local on the 1st
	if n := c.settle(s); n != 0 {
		t.Fatalf("settled %d merchants before the 2nd, want 0", n)
	}

	c.toLocalDay(2)
	if n := c.settle(s); n != 1 {
		t.Fatalf("settled %d merchants on the 2nd, want 1", n)
	}
	for range 3 {
		if n := c.settle(s); n != 0 {
			t.Fatalf("settled %d merchants again for the same cutoff, want 0", n)
		}
	}
	c.clock.Advance(23 * time.Hour)
	if n := c.settle(s); n != 0 {
		t.Fatalf("settled %d merchants later on the 2nd, want 0", n)
	}

	got := c.settlements()
	if len(got) != 1 || got[0].amount != 1940 || !got[0].cutoff.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) ||
		!slices.Equal(got[0].payments, []string{id}) {
		t.Fatalf("settlements = %+v, want one of 1940 on the 2nd with %s", got, id)
	}
	if b := c.balance("merchant_payable:"); b != 0 {
		t.Errorf("merchant payable = %d, want 0", b)
	}
	if b := c.balance("settlements_paid:"); b != 1940 {
		t.Errorf("settlements paid = %d, want 1940", b)
	}
}

// Workers racing on the same cutoff: exactly one pays, with every available payment.
func TestConcurrentSettlementsPayOnce(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	ids := []string{c.paidPayment(key), c.paidPayment(key), c.paidPayment(key)}
	c.toLocalDay(2)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		paid int
	)
	for range 8 {
		s := c.settler()
		wg.Go(func() {
			n, err := s.Settle(t.Context())
			if err != nil {
				t.Errorf("Settle() error = %v", err)
			}
			mu.Lock()
			paid += n
			mu.Unlock()
		})
	}
	wg.Wait()

	if paid != 1 {
		t.Errorf("%d settlements reported, want 1", paid)
	}
	got := c.settlements()
	if len(got) != 1 || got[0].amount != 3*1940 || !slices.Equal(got[0].payments, sorted(ids...)) {
		t.Fatalf("settlements = %+v, want one of %d with %v", got, 3*1940, sorted(ids...))
	}
	if b := c.balance("merchant_payable:"); b != 0 {
		t.Errorf("merchant payable = %d, want 0", b)
	}
}

// A refund of a payment not available yet comes out of that payment's net; one of a
// payment already settled comes out of the next settlement (section 4.7).
func TestRefundsComeOutOfTheRightSettlement(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	s := c.settler()

	a := c.paidPayment(key)
	c.refund(key, a, 500) // A is still pending: its net becomes 1440
	// A refund the provider refuses gives its hold back and takes nothing.
	if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.Reject, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+a, "rejected", usdBody(300)), true)); r.Status != "failed" {
		t.Fatalf("refused refund = %+v, want failed", r)
	}
	c.toLocalDay(2)
	c.settle(s)

	c.refund(key, a, 1000) // A is settled: the 1000 come out of what follows
	b := c.paidPayment(key)
	c.toLocalDay(3)
	c.settle(s)

	got := c.settlements()
	if len(got) != 2 || got[0].amount != 1440 || !slices.Equal(got[0].payments, []string{a}) ||
		got[1].amount != 940 || !slices.Equal(got[1].payments, []string{b}) {
		t.Fatalf("settlements = %+v, want 1440 with A on the 2nd and 940 with B on the 3rd", got)
	}
	if bal := c.balance("merchant_payable:"); bal != 0 {
		t.Errorf("merchant payable = %d, want 0", bal)
	}
}

// Money paid and refunded on the day of the run is pending: neither the payment nor
// its refund changes what the run pays, and both arrive in the next one.
func TestPendingPaymentsAndTheirRefundsWait(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	s := c.settler()

	a := c.paidPayment(key)
	c.toLocalDay(2)
	b := c.paidPayment(key)
	c.refund(key, b, 1000)
	// Refused, it gives its hold back: it takes nothing from B's net.
	if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.Reject, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+b, "rejected", usdBody(300)), true)); r.Status != "failed" {
		t.Fatalf("refused refund = %+v, want failed", r)
	}
	c.settle(s)
	if b := c.balance("merchant_payable:"); b != 940 {
		t.Errorf("merchant payable after the 2nd = %d, want B's 940 left pending", b)
	}

	c.toLocalDay(3)
	c.settle(s)
	got := c.settlements()
	if len(got) != 2 || got[0].amount != 1940 || !slices.Equal(got[0].payments, []string{a}) ||
		got[1].amount != 940 || !slices.Equal(got[1].payments, []string{b}) {
		t.Fatalf("settlements = %+v, want 1940 with A on the 2nd and 940 with B on the 3rd", got)
	}
}

// Decision 27 for F7: what the merchant owes after a refund of settled money is not
// paid out as a negative settlement; it waits, and the payments that arrive meanwhile
// go into the first settlement that is positive again.
func TestNegativeAvailableWaitsForMoreMoney(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	s := c.settler()

	a := c.paidPayment(key)
	c.toLocalDay(2)
	c.settle(s)
	c.refund(key, a, 2000) // the whole payment back, the fee not returned: -2000

	c.toLocalDay(3)
	b := c.paidPayment(key)
	if n := c.settle(s); n != 0 {
		t.Fatalf("settled %d with -2000 available, want 0", n)
	}
	c.toLocalDay(4)
	if n := c.settle(s); n != 0 {
		t.Fatalf("settled %d with -60 available, want 0", n)
	}
	cc := c.paidPayment(key)
	c.toLocalDay(5)
	if n := c.settle(s); n != 1 {
		t.Fatalf("settled %d with 1880 available, want 1", n)
	}

	got := c.settlements()
	if len(got) != 2 || got[1].amount != 1880 || !slices.Equal(got[1].payments, sorted(b, cc)) {
		t.Fatalf("settlements = %+v, want the second of 1880 with B and C", got)
	}
}

// Payments and refunds at random over several days, against a model of what each
// cutoff should pay: the sum of the nets of the payments available by it, less what
// was paid before; nothing when that is not positive. Each payment is in exactly the
// first settlement after it became available.
func TestSettlementsMatchAModel(t *testing.T) {
	for round := range 4 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			rng := mrand.New(mrand.NewPCG(uint64(round), 7)) //nolint:gosec // G404: reproducible rounds, nothing secret.
			c := newContract(t)
			key := c.merchantKey()
			s := c.settler()

			type modelPayment struct {
				id       string
				day      int
				net      int64
				refunded int64
				settled  bool
			}
			var all []*modelPayment
			var paidOut int64
			wantSettlements := 0
			activity := func(day int) {
				for range rng.IntN(3) {
					all = append(all, &modelPayment{id: c.paidPayment(key), day: day, net: 1940})
				}
				for range rng.IntN(3) {
					if len(all) == 0 {
						break
					}
					p := all[rng.IntN(len(all))]
					if left := 2000 - p.refunded; left > 0 {
						cents := 1 + rng.Int64N(left)
						c.refund(key, p.id, cents)
						p.refunded += cents
						p.net -= cents
					}
				}
			}
			for day := 1; day <= 6; day++ {
				if day > 1 {
					c.toLocalDay(day)
					// Paid and refunded before the run: pending today, not available.
					activity(day)
					var available int64
					var included []string
					for _, p := range all {
						if p.day < day {
							available += p.net
							if !p.settled {
								included = append(included, p.id)
							}
						}
					}
					available -= paidOut
					// Run it twice: the second is a no-op.
					n := c.settle(s) + c.settle(s)
					if available > 0 {
						if n != 1 {
							t.Fatalf("day %d: %d settlements with %d available, want 1", day, n, available)
						}
						wantSettlements++
						got := c.settlements()
						last := got[len(got)-1]
						if last.amount != available || !slices.Equal(last.payments, sorted(included...)) {
							t.Fatalf("day %d: settlement %+v, want %d with %v", day, last, available, sorted(included...))
						}
						paidOut += available
						for _, p := range all {
							if p.day < day {
								p.settled = true
							}
						}
					} else if n != 0 {
						t.Fatalf("day %d: %d settlements with %d available, want 0", day, n, available)
					}
				}
				activity(day)
			}
			if wantSettlements == 0 {
				t.Fatal("no settlement in the whole round: the model tested nothing")
			}
			if got := len(c.settlements()); got != wantSettlements {
				t.Errorf("%d settlements, want %d", got, wantSettlements)
			}
			c.checkBooksBalance()
		})
	}
}

// checkBooksBalance is invariant 1 over the whole ledger: every currency sums to zero.
func (c *contract) checkBooksBalance() {
	c.t.Helper()
	var unbalanced int
	err := c.pool.QueryRow(c.t.Context(), `SELECT count(*) FROM (SELECT currency FROM ledger_entries
		GROUP BY currency HAVING sum(amount) <> 0) AS s`).Scan(&unbalanced)
	if err != nil || unbalanced != 0 {
		c.t.Errorf("%d currencies do not sum to zero (%v)", unbalanced, err)
	}
}

// Settlements running while payments are confirmed and refunded: no lock cycle with
// the paths that take the payment and then the payable, and once everything is
// available every cent is paid out exactly once.
func TestSettlementUnderConcurrentPaymentsAndRefunds(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	var settled []string
	for range 6 {
		settled = append(settled, c.paidPayment(key))
	}
	c.toLocalDay(2)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var fresh []string
	for range 4 {
		s := c.settler()
		wg.Go(func() {
			for range 5 {
				if _, err := s.Settle(t.Context()); err != nil {
					t.Errorf("Settle() error = %v", err)
				}
			}
		})
	}
	for _, id := range settled {
		wg.Go(func() {
			rec := c.do(postRefund(t, key, "/v1/payments/"+id, "r-"+id, usdBody(700)), true)
			if rec.Code != 201 {
				t.Errorf("refund of %s: %d %s", id, rec.Code, rec.Body)
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			id := c.paidPayment(key)
			mu.Lock()
			fresh = append(fresh, id)
			mu.Unlock()
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	c.toLocalDay(3)
	c.settle(c.settler())
	var total int64
	for _, s := range c.settlements() {
		total += s.amount
	}
	// The six first, the four fresh, less six refunds of 700: everything is available.
	if want := int64(10*1940 - 6*700); total != want {
		t.Errorf("paid out %d in all, want %d", total, want)
	}
	if b := c.balance("merchant_payable:"); b != 0 {
		t.Errorf("merchant payable = %d, want 0", b)
	}
	var unsettled int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM payments WHERE settlement_id IS NULL").Scan(&unsettled); err != nil || unsettled != 0 {
		t.Errorf("%d payments in no settlement (%v), want 0", unsettled, err)
	}
	c.checkBooksBalance()
}
