package adapters_test

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

// countingGets counts the lookups that reach the provider.
type countingGets struct {
	app.Provider
	gets atomic.Int64
}

func (p *countingGets) GetAttempt(ctx context.Context, id domain.AttemptID) (app.AttemptResult, error) {
	p.gets.Add(1)
	return p.Provider.GetAttempt(ctx, id) //nolint:wrapcheck // a test double passes the provider's error through untouched.
}

type resolverEnv struct {
	t        *testing.T
	pool     *pgxpool.Pool
	store    *adapters.Postgres
	sim      *pixsim.Client
	clock    *clock.Fake
	provider *countingGets
	resolver *app.Resolver
}

func newResolverEnv(t *testing.T) *resolverEnv {
	t.Helper()
	pool := pgtest.NewPool(t)
	store := adapters.NewPostgres(pool, sinks())
	pix, sim := newPixSim(t)
	c := clock.NewFake(t0)
	provider := &countingGets{Provider: pix}
	return &resolverEnv{
		t: t, pool: pool, store: store, sim: sim, clock: c, provider: provider,
		resolver: app.NewResolver(store, provider, c, slog.New(slog.DiscardHandler), 200*time.Millisecond),
	}
}

// attempt stores a payment with an attempt moved by events at t0. charge also creates
// it at the provider, as a create whose answer was lost.
func (e *resolverEnv) attempt(charge bool, events ...domain.AttemptEvent) domain.AttemptID {
	e.t.Helper()
	a := create(e.t, e.store, newPayment(e.t, e.pool))
	apply(e.t, e.store, a.ID(), events...)
	if charge {
		req := pixsim.CreateCharge{Amount: money.New(10150, money.BRL), ExpiresAt: t0.Add(domain.PixQuoteValidity)}
		if _, err := e.sim.Create(e.t.Context(), a.ID().String(), req); err != nil {
			e.t.Fatal(err)
		}
	}
	return a.ID()
}

func (e *resolverEnv) resolve() int {
	e.t.Helper()
	n, err := e.resolver.Resolve(e.t.Context(), app.ResolveBatch)
	if err != nil {
		e.t.Fatalf("Resolve() error = %v", err)
	}
	return n
}

type schedule struct {
	status    string
	checks    int
	next      *time.Time
	escalated *time.Time
}

func (e *resolverEnv) schedule(id domain.AttemptID) schedule {
	e.t.Helper()
	var s schedule
	err := e.pool.QueryRow(e.t.Context(),
		"SELECT status, checks, next_check_at, escalated_at FROM payment_attempts WHERE id = $1", id).
		Scan(&s.status, &s.checks, &s.next, &s.escalated)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func TestResolverAcceptsAChargeTheProviderCreated(t *testing.T) {
	e := newResolverEnv(t)
	id := e.attempt(true, domain.Send, domain.Ambiguous)

	e.clock.Advance(domain.CheckBase - time.Second)
	if n := e.resolve(); n != 0 {
		t.Fatalf("claimed %d attempts before the first check was due", n)
	}
	e.clock.Advance(time.Second)
	if n := e.resolve(); n != 1 {
		t.Fatalf("claimed %d attempts, want 1", n)
	}
	a, err := e.store.GetAttempt(t.Context(), id)
	if err != nil || a.Status() != domain.AttemptAccepted || a.ExternalID() == "" || a.BRCode() == "" {
		t.Fatalf("attempt = %+v, %v; want accepted with the charge's ID and BR Code", a, err)
	}
	if s := e.schedule(id); s.next != nil {
		t.Errorf("resolved attempt still scheduled at %v", s.next)
	}
	if n := e.resolve(); n != 0 {
		t.Errorf("claimed %d attempts after the only one was resolved", n)
	}
}

func TestResolverSeesAPaymentMadeMeanwhile(t *testing.T) {
	e := newResolverEnv(t)
	id := e.attempt(true, domain.Send, domain.Ambiguous)
	charge, err := e.sim.Get(t.Context(), id.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.sim.Pay(t.Context(), charge.ID); err != nil {
		t.Fatal(err)
	}

	e.clock.Advance(domain.CheckBase)
	e.resolve()
	if a, _ := e.store.GetAttempt(t.Context(), id); a.Status() != domain.AttemptSucceeded || a.BRCode() == "" {
		t.Errorf("attempt = %s with code %q, want succeeded with the BR Code", a.Status(), a.BRCode())
	}
}

// Not found is ambiguous at first, since the create may still be on its way, and a
// refusal once nothing can reach the provider anymore.
func TestResolverRejectsWhatTheProviderNeverGot(t *testing.T) {
	e := newResolverEnv(t)
	id := e.attempt(false, domain.Send, domain.Ambiguous)

	e.clock.Advance(domain.CheckBase)
	e.resolve()
	s := e.schedule(id)
	if s.status != "unknown" || s.checks != 1 || s.next == nil || !s.next.Equal(e.clock.Now().Add(domain.CheckBackoff(1))) {
		t.Fatalf("within the grace: %+v, want unknown, 1 check, next in %v", s, domain.CheckBackoff(1))
	}

	e.clock.Advance(domain.NotFoundGrace - domain.CheckBase)
	e.resolve()
	if s := e.schedule(id); s.status != "rejected" || s.next != nil {
		t.Errorf("after the grace: %+v, want rejected and out of the queue", s)
	}
}

// Decision 52: 5 s, 10 s, 20 s… at most 5 min between lookups, and after 24 h the
// attempt goes to a person and is never looked up again.
func TestResolverBacksOffAndEscalates(t *testing.T) {
	e := newResolverEnv(t)
	id := e.attempt(true, domain.Send, domain.Ambiguous)
	if err := e.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpGet, Kind: pixsim.FailBefore, Count: 1000}); err != nil {
		t.Fatal(err)
	}

	var waits []time.Duration
	last := t0
	for {
		s := e.schedule(id)
		if s.next == nil {
			break
		}
		if len(waits) > 1000 {
			t.Fatalf("still scheduled after %d checks and %v", len(waits), e.clock.Now().Sub(t0))
		}
		waits = append(waits, s.next.Sub(last))
		last = *s.next
		e.clock.Advance(s.next.Sub(e.clock.Now()))
		e.resolve()
	}

	for i, w := range waits {
		if want := domain.CheckBackoff(i); w != want {
			t.Fatalf("wait %d = %v, want %v; waits %v", i, w, want, waits[:i+1])
		}
	}
	s := e.schedule(id)
	if s.status != "unknown" || s.escalated == nil || e.clock.Now().Sub(t0) < domain.ReviewAfter {
		t.Fatalf("final %+v at %v, want unknown and escalated after %v", s, e.clock.Now().Sub(t0), domain.ReviewAfter)
	}
	lookups := e.provider.gets.Load()
	if lookups != int64(len(waits)-1) {
		t.Errorf("%d lookups for %d scheduled checks; the last one must escalate without a lookup", lookups, len(waits))
	}
	e.clock.Advance(time.Hour)
	if n := e.resolve(); n != 0 || e.provider.gets.Load() != lookups {
		t.Errorf("an escalated attempt was claimed again")
	}
	t.Logf("%d lookups over %v before escalating", lookups, last.Sub(t0))
}

// D1: a request that wrote sent and died leaves no one to retry; once stale, the
// resolver takes it like an unknown one.
func TestResolverTakesStaleSentAttempts(t *testing.T) {
	e := newResolverEnv(t)
	id := e.attempt(true, domain.Send)
	if s := e.schedule(id); s.next == nil || !s.next.Equal(t0.Add(domain.SentStaleAfter)) {
		t.Fatalf("sent attempt scheduled at %v, want %v", s.next, t0.Add(domain.SentStaleAfter))
	}

	e.clock.Advance(domain.SentStaleAfter - time.Second)
	if n := e.resolve(); n != 0 {
		t.Fatalf("claimed %d sent attempts before they went stale", n)
	}
	e.clock.Advance(time.Second)
	if n := e.resolve(); n != 1 {
		t.Fatalf("claimed %d attempts, want 1", n)
	}
	if s := e.schedule(id); s.status != "accepted" || s.next != nil {
		t.Errorf("stale sent with a charge: %+v, want accepted and out of the queue", s)
	}
}

// A stale sent whose lookup fails becomes unknown and starts the backoff from zero.
func TestStaleSentWithAFailedLookupBecomesUnknown(t *testing.T) {
	e := newResolverEnv(t)
	id := e.attempt(true, domain.Send)
	if err := e.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpGet, Kind: pixsim.FailBefore, Count: 1}); err != nil {
		t.Fatal(err)
	}

	e.clock.Advance(domain.SentStaleAfter)
	e.resolve()
	s := e.schedule(id)
	if s.status != "unknown" || s.checks != 0 || s.next == nil || !s.next.Equal(e.clock.Now().Add(domain.CheckBase)) {
		t.Fatalf("after a failed lookup: %+v, want unknown, 0 checks, next in %v", s, domain.CheckBase)
	}
	e.clock.Advance(domain.CheckBase)
	e.resolve()
	if s := e.schedule(id); s.status != "accepted" {
		t.Errorf("next lookup left it %s, want accepted", s.status)
	}
}

// The lease, not a lock, keeps a claimed attempt from a second worker while the
// provider is called; a worker that dies holding it only delays the attempt.
func TestClaimLeaseExpires(t *testing.T) {
	e := newResolverEnv(t)
	id := e.attempt(true, domain.Send, domain.Ambiguous)
	e.clock.Advance(domain.CheckBase)

	claimed, err := e.store.ClaimDueAttempts(t.Context(), e.clock.Now(), e.clock.Now().Add(app.ClaimLease), 10)
	if err != nil || len(claimed) != 1 || claimed[0].Attempt.ID() != id {
		t.Fatalf("ClaimDueAttempts() = %+v, %v; want the attempt", claimed, err)
	}
	if n := e.resolve(); n != 0 {
		t.Fatalf("claimed %d attempts while the lease holds", n)
	}
	e.clock.Advance(app.ClaimLease)
	if n := e.resolve(); n != 1 {
		t.Errorf("claimed %d attempts after the lease ended, want 1", n)
	}
}

func TestConcurrentResolversLookEachAttemptUpOnce(t *testing.T) {
	e := newResolverEnv(t)
	const attempts, resolvers = 50, 4
	ids := make([]domain.AttemptID, attempts)
	for i := range ids {
		ids[i] = e.attempt(true, domain.Send, domain.Ambiguous)
	}
	e.clock.Advance(domain.CheckBase)

	var claimed atomic.Int64
	var wg sync.WaitGroup
	for range resolvers {
		wg.Go(func() {
			for {
				n, err := e.resolver.Resolve(t.Context(), app.ResolveBatch)
				if err != nil {
					t.Errorf("Resolve() error = %v", err)
					return
				}
				if n == 0 {
					return
				}
				claimed.Add(int64(n))
			}
		})
	}
	wg.Wait()

	if c, g := claimed.Load(), e.provider.gets.Load(); c != attempts || g != attempts {
		t.Errorf("%d claims and %d lookups for %d attempts, want one of each per attempt", c, g, attempts)
	}
	for _, id := range ids {
		if s := e.schedule(id); s.status != "accepted" {
			t.Errorf("attempt %s ended %s, want accepted", id, s.status)
		}
	}
}
