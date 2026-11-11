package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	fxadapters "github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

// refundResolver is the worker's refund-resolver job against the contract's database,
// provider and clock.
func (c *contract) refundResolver() *paymentsapp.RefundResolver {
	c.t.Helper()
	rate, err := fx.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		c.t.Fatal(err)
	}
	store := paymentsadapters.NewPostgres(c.pool, paymentSinks())
	refunder := paymentsapp.NewRefunder(fxadapters.NewFixedRates(rate), store, c.provider, c.clock, slog.New(slog.DiscardHandler), providerTimeout)
	return paymentsapp.NewRefundResolver(refunder, store)
}

// sentFirst is a store where, the first time a refund is sent, another path sends it
// a moment before: the retried request winning the race against the resolver.
type sentFirst struct {
	*paymentsadapters.Postgres
	once sync.Once
}

func (s *sentFirst) ApplyRefund(ctx context.Context, id payments.RefundID, u *payments.RefundUpdate, rate fx.Rate, now time.Time) (payments.Refund, bool, error) {
	if u.Event == payments.SendRefund {
		var err error
		s.once.Do(func() { _, _, err = s.Postgres.ApplyRefund(ctx, id, u, rate, now) })
		if err != nil {
			return payments.Refund{}, false, err
		}
	}
	return s.Postgres.ApplyRefund(ctx, id, u, rate, now) //nolint:wrapcheck // a test double passes the store's error through untouched.
}

func (c *contract) resolveRefunds(r *paymentsapp.RefundResolver) int {
	c.t.Helper()
	n, err := r.Resolve(c.t.Context(), paymentsapp.ResolveBatch)
	if err != nil {
		c.t.Fatalf("Resolve() error = %v", err)
	}
	return n
}

// refundSchedule is the only refund's row as the queue sees it.
func (c *contract) refundSchedule() schedule {
	c.t.Helper()
	var s schedule
	err := c.pool.QueryRow(c.t.Context(), "SELECT status, checks, next_check_at, escalated_at FROM refunds").
		Scan(&s.status, &s.checks, &s.next, &s.escalated)
	if err != nil {
		c.t.Fatal(err)
	}
	return s
}

type schedule struct {
	status    string
	checks    int
	next      *time.Time
	escalated *time.Time
}

func (c *contract) paymentStatus() string {
	c.t.Helper()
	var status string
	if err := c.pool.QueryRow(c.t.Context(), "SELECT status FROM payments").Scan(&status); err != nil {
		c.t.Fatal(err)
	}
	return status
}

// Decision 17 for refunds: the provider returned the money but the answer was lost.
// Nobody retries the request; the resolver looks the refund up and finishes it.
func TestRefundResolverFinishesARefundWhoseAnswerWasLost(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.TimeoutAfter, Count: 1}); err != nil {
		t.Fatal(err)
	}
	r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(1000)), true))
	if r.Status != "pending" {
		t.Fatalf("refund = %+v, want pending", r)
	}

	resolver := c.refundResolver()
	c.clock.Advance(payments.CheckBase - time.Second)
	if n := c.resolveRefunds(resolver); n != 0 {
		t.Fatalf("claimed %d refunds before the first check was due", n)
	}
	c.clock.Advance(time.Second)
	if n := c.resolveRefunds(resolver); n != 1 {
		t.Fatalf("claimed %d refunds, want 1", n)
	}
	if s := c.refundSchedule(); s.status != "succeeded" || s.next != nil {
		t.Fatalf("refund %+v, want succeeded and out of the queue", s)
	}
	if n := c.provider.refunds.Load(); n != 1 {
		t.Errorf("provider asked for the refund %d times, want 1: a lookup never sends again", n)
	}
	if got := c.paymentStatus(); got != "partially_refunded" {
		t.Errorf("payment = %s, want partially_refunded", got)
	}
	c.checkLedger(1000, 5075)
	c.checkRefundEvent("refund.succeeded", r.ID)
	if n := c.resolveRefunds(resolver); n != 0 {
		t.Errorf("claimed %d refunds after the only one was resolved", n)
	}
}

// A request that died after its first transaction left the refund created with its
// hold: the provider never heard of it, so the resolver sends it, once, and a later
// retry of the request replays the result.
func TestRefundResolverSendsARefundLeftCreated(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	_, crashed := c.crashedRefund(key, id, "r1", 1000)

	resolver := c.refundResolver()
	c.clock.Advance(payments.SentStaleAfter - time.Second)
	if n := c.resolveRefunds(resolver); n != 0 {
		t.Fatalf("claimed %d refunds while the request could still be sending", n)
	}
	c.clock.Advance(time.Second)
	if n := c.resolveRefunds(resolver); n != 1 {
		t.Fatalf("claimed %d refunds, want 1", n)
	}
	if s := c.refundSchedule(); s.status != "succeeded" || s.next != nil {
		t.Fatalf("refund %+v, want succeeded and out of the queue", s)
	}
	c.checkLedger(1000, 5075)

	r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(1000)), true))
	if r.ID != crashed.ID().String() || r.Status != "succeeded" {
		t.Errorf("retried request = %+v, want %s succeeded", r, crashed.ID())
	}
	if n := c.provider.refunds.Load(); n != 1 || c.count("refunds") != 1 {
		t.Errorf("provider called %d times for %d refunds, want 1 and 1", n, c.count("refunds"))
	}
}

// The provider failed before storing the refund, so the money never went. Not found
// is trusted only after NotFoundGrace from the refund's last change; then the hold goes
// back to the merchant and the payment stays paid.
func TestRefundResolverReleasesWhatTheProviderNeverGot(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.FailBefore, Count: 1}); err != nil {
		t.Fatal(err)
	}
	r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(1000)), true))
	if r.Status != "pending" {
		t.Fatalf("refund = %+v, want pending", r)
	}

	resolver := c.refundResolver()
	c.clock.Advance(payments.CheckBase)
	c.resolveRefunds(resolver)
	if s := c.refundSchedule(); s.status != "unknown" || s.checks != 1 {
		t.Fatalf("refund %+v inside the grace, want unknown after 1 failed check", s)
	}
	if got := c.balance("refunds_in_flight:"); got != 1000 {
		t.Errorf("in flight = %d inside the grace, want the 1000 held", got)
	}

	c.clock.Advance(payments.NotFoundGrace)
	c.resolveRefunds(resolver)
	if s := c.refundSchedule(); s.status != "rejected" || s.next != nil {
		t.Fatalf("refund %+v after the grace, want rejected and out of the queue", s)
	}
	c.checkLedger(0, 0)
	c.checkRefundEvent("refund.failed", r.ID)
	if got := c.paymentStatus(); got != "paid" {
		t.Errorf("payment = %s, want paid", got)
	}
}

// Lookups that keep failing back off as the attempts' do and, after ReviewAfter, the
// refund goes to a person with its hold kept: the money may be gone.
func TestRefundResolverBacksOffAndEscalates(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.TimeoutAfter, Count: 1}); err != nil {
		t.Fatal(err)
	}
	refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(1000)), true))
	if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpGetRefund, Kind: pixsim.FailBefore, Count: 1000}); err != nil {
		t.Fatal(err)
	}

	resolver := c.refundResolver()
	c.clock.Advance(payments.CheckBase)
	// Five checks take past NotFoundGrace: a provider that fails to answer is not one
	// that does not know the refund, so the refund is never rejected for it.
	for failed := 1; failed <= 5; failed++ {
		if n := c.resolveRefunds(resolver); n != 1 {
			t.Fatalf("check %d claimed %d refunds, want 1", failed, n)
		}
		s := c.refundSchedule()
		want := c.clock.Now().Add(payments.CheckBackoff(failed))
		if s.status != "unknown" || s.checks != failed || s.next == nil || !s.next.Equal(want) {
			t.Fatalf("after %d failed checks: %+v, want unknown, next at %v", failed, s, want)
		}
		c.clock.Advance(payments.CheckBackoff(failed))
	}

	c.clock.Advance(t0.Add(payments.ReviewAfter).Sub(c.clock.Now()))
	c.resolveRefunds(resolver)
	if s := c.refundSchedule(); s.status != "unknown" || s.next != nil || s.escalated == nil {
		t.Fatalf("refund %+v after the review limit, want unknown, escalated and out of the queue", s)
	}
	if got := c.balance("refunds_in_flight:"); got != 1000 {
		t.Errorf("in flight = %d, want the 1000 held for the person to decide", got)
	}
	if n := c.resolveRefunds(resolver); n != 0 {
		t.Errorf("claimed %d refunds after escalation", n)
	}
}

// Workers and a retried request reaching the same created refund at once: only the
// one that moves it to sent calls the provider.
func TestConcurrentRefundResolversSendOnce(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	c.crashedRefund(key, id, "r1", 1000)
	c.clock.Advance(payments.SentStaleAfter)

	var wg sync.WaitGroup
	for range 4 {
		resolver := c.refundResolver()
		wg.Go(func() {
			if _, err := resolver.Resolve(t.Context(), paymentsapp.ResolveBatch); err != nil {
				t.Errorf("Resolve() error = %v", err)
			}
		})
	}
	var code int
	wg.Go(func() { code = c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(1000)), true).Code })
	wg.Wait()

	if code != http.StatusCreated {
		t.Errorf("retried request = %d, want 201", code)
	}
	if n := c.provider.refunds.Load(); n != 1 {
		t.Errorf("provider asked for the refund %d times, want 1", n)
	}
	if s := c.refundSchedule(); s.status != "succeeded" {
		t.Errorf("refund %+v, want succeeded", s)
	}
	c.checkLedger(1000, 5075)
}

// The resolver claimed a created refund, but another path sent it first: the resolver
// must not call the provider, since the other one does.
func TestRefundSentByAnotherPathIsNotSentAgain(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	c.crashedRefund(key, id, "r1", 1000)
	c.clock.Advance(payments.SentStaleAfter)

	rate, err := fx.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		t.Fatal(err)
	}
	store := &sentFirst{Postgres: paymentsadapters.NewPostgres(c.pool, paymentSinks())}
	refunder := paymentsapp.NewRefunder(fxadapters.NewFixedRates(rate), store, c.provider, c.clock, slog.New(slog.DiscardHandler), providerTimeout)
	if n := c.resolveRefunds(paymentsapp.NewRefundResolver(refunder, store.Postgres)); n != 1 {
		t.Fatalf("claimed %d refunds, want 1", n)
	}
	if n := c.provider.refunds.Load(); n != 0 {
		t.Errorf("provider asked %d times by the resolver that lost the race, want 0", n)
	}
	if s := c.refundSchedule(); s.status != "sent" {
		t.Errorf("refund %+v, want sent, left to the path that sent it", s)
	}
}

// The resolver sent a refund left created long after its creation, and the provider
// failed before storing it. Not found is not trusted until NotFoundGrace after the
// send, however old the refund is: the call could still be on its way.
func TestRefundNotFoundGraceRunsFromTheSend(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	c.crashedRefund(key, id, "r1", 1000)
	if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.FailBefore, Count: 1}); err != nil {
		t.Fatal(err)
	}

	resolver := c.refundResolver()
	c.clock.Advance(payments.SentStaleAfter + payments.NotFoundGrace)
	c.resolveRefunds(resolver)
	if s := c.refundSchedule(); s.status != "unknown" {
		t.Fatalf("refund %+v after the late send, want unknown", s)
	}
	c.clock.Advance(payments.CheckBase)
	c.resolveRefunds(resolver)
	if s := c.refundSchedule(); s.status != "unknown" || s.checks != 1 {
		t.Errorf("refund %+v %v after the send, want still unknown inside the grace", s, payments.CheckBase)
	}
}

// A request that died after sending: the money may be gone, so the resolver never
// sends again. The stale sent becomes unknown and is looked up from then on.
func TestRefundResolverTakesStaleSentRefunds(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	store, crashed := c.crashedRefund(key, id, "r1", 1000)
	rate, err := fx.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ApplyRefund(t.Context(), crashed.ID(), &payments.RefundUpdate{Event: payments.SendRefund}, rate, c.clock.Now()); err != nil {
		t.Fatal(err)
	}

	resolver := c.refundResolver()
	c.clock.Advance(payments.SentStaleAfter)
	if n := c.resolveRefunds(resolver); n != 1 {
		t.Fatalf("claimed %d refunds, want the stale sent one", n)
	}
	s := c.refundSchedule()
	if want := c.clock.Now().Add(payments.CheckBase); s.status != "unknown" || s.next == nil || !s.next.Equal(want) {
		t.Errorf("refund %+v, want unknown with its first check at %v", s, want)
	}
	if n := c.provider.refunds.Load(); n != 0 {
		t.Errorf("provider asked for the refund %d times, want 0", n)
	}
}
