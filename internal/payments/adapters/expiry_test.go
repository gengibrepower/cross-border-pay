package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

// expiresAt is when the quotes of newPayment expire.
var expiresAt = t0.Add(domain.PixQuoteValidity)

// expiryEnv runs the simulator and the expirer on one fake clock, so the time the
// simulator records for a payment is the time the test chose.
type expiryEnv struct {
	t        *testing.T
	pool     *pgxpool.Pool
	store    *adapters.Postgres
	sim      *pixsim.Client
	clock    *clock.Fake
	provider *countingGets
	expirer  *app.Expirer
}

func newExpiryEnv(t *testing.T) *expiryEnv {
	t.Helper()
	pool := pgtest.NewPool(t)
	c := clock.NewFake(t0)
	pix, sim := newPixSimWith(t, c)
	store := adapters.NewPostgres(pool, sqlOutbox{})
	provider := &countingGets{Provider: pix}
	return &expiryEnv{
		t: t, pool: pool, store: store, sim: sim, clock: c, provider: provider,
		expirer: app.NewExpirer(store, provider, c, slog.New(slog.DiscardHandler), 200*time.Millisecond),
	}
}

// payment stores a payment whose attempt is accepted with a charge at the simulator.
func (e *expiryEnv) payment() (domain.PaymentID, domain.AttemptID, pixsim.Charge) {
	e.t.Helper()
	payment := newPayment(e.t, e.pool)
	a := create(e.t, e.store, payment)
	ch, err := e.sim.Create(e.t.Context(), a.ID().String(), pixsim.CreateCharge{Amount: money.New(10150, money.BRL), ExpiresAt: expiresAt})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, u := range []domain.AttemptUpdate{{Event: domain.Send}, {Event: domain.Accept, ExternalID: ch.ID, BRCode: ch.BRCode}} {
		if _, _, err := e.store.ApplyAttempt(e.t.Context(), a.ID(), u, t0); err != nil {
			e.t.Fatal(err)
		}
	}
	return payment, a.ID(), ch
}

func (e *expiryEnv) at(at time.Time) { e.clock.Advance(at.Sub(e.clock.Now())) }

func (e *expiryEnv) pay(ch *pixsim.Charge) pixsim.Charge {
	e.t.Helper()
	paid, err := e.sim.Pay(e.t.Context(), ch.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return paid
}

func (e *expiryEnv) expire() int {
	e.t.Helper()
	n, err := e.expirer.Expire(e.t.Context(), app.ExpiryBatch)
	if err != nil {
		e.t.Fatalf("Expire() error = %v", err)
	}
	return n
}

type paymentState struct {
	status  string
	version int
	events  []string
	posted  int
}

func (e *expiryEnv) state(id domain.PaymentID) paymentState {
	e.t.Helper()
	var s paymentState
	err := e.pool.QueryRow(e.t.Context(), `SELECT status, version,
		coalesce((SELECT array_agg(type ORDER BY id) FROM outbox_events WHERE subject_id = $2), '{}'),
		(SELECT count(*) FROM ledger_transactions WHERE reference = $3)
		FROM payments WHERE id = $1`, id, id.String(), domain.ConfirmationReference(id)).
		Scan(&s.status, &s.version, &s.events, &s.posted)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func TestExpiresAnUnpaidPayment(t *testing.T) {
	e := newExpiryEnv(t)
	id, attempt, _ := e.payment()

	e.at(expiresAt.Add(-time.Microsecond))
	if n := e.expire(); n != 0 {
		t.Fatalf("claimed %d payments before the quote expired", n)
	}
	e.at(expiresAt)
	if n := e.expire(); n != 1 {
		t.Fatalf("claimed %d payments, want 1", n)
	}
	got := e.state(id)
	if got.status != "expired" || got.version != 2 || !slices.Equal(got.events, []string{"payment.expired"}) || got.posted != 0 {
		t.Errorf("payment %+v; want expired, version 2, payment.expired and nothing posted", got)
	}
	if a, err := e.store.GetAttempt(t.Context(), attempt); err != nil || a.Status() != domain.AttemptAccepted {
		t.Errorf("attempt %s (%v), want it left accepted", a.Status(), err)
	}
	if n := e.expire(); n != 0 {
		t.Errorf("claimed %d payments after expiring them, want 0", n)
	}
}

// The race of section 4.6: the payer pays two seconds before the quote expires, and
// the news has not arrived when the job runs. The job asks the provider, finds the
// payment and confirms it: the payer paid on time. When the event comes, it is a no-op.
func TestPaidOnTimeIsConfirmedEvenIfTheNewsIsLate(t *testing.T) {
	e := newExpiryEnv(t)
	id, attempt, ch := e.payment()
	e.at(expiresAt.Add(-2 * time.Second))
	paid := e.pay(&ch)

	e.at(expiresAt)
	e.expire()
	got := e.state(id)
	if got.status != "paid" || got.version != 2 || !slices.Equal(got.events, []string{"payment.paid"}) || got.posted != 1 {
		t.Fatalf("payment %+v; want paid, version 2, payment.paid and one posting", got)
	}

	e.at(expiresAt.Add(3 * time.Second))
	event := pixsim.Event{ID: "evt_1", Type: pixsim.ChargePaid, CreatedAt: *paid.PaidAt, Charge: paid}
	if _, err := e.handle(&event); err != nil {
		t.Fatalf("late news of an on-time payment: %v", err)
	}
	if after := e.state(id); after.version != 2 || len(after.events) != 1 || after.posted != 1 {
		t.Errorf("payment %+v after the event, want it unchanged", after)
	}
	if a, err := e.store.GetAttempt(t.Context(), attempt); err != nil || a.Status() != domain.AttemptSucceeded {
		t.Errorf("attempt %s (%v), want succeeded", a.Status(), err)
	}
}

func (e *expiryEnv) handle(event *pixsim.Event) (string, error) {
	e.t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		e.t.Fatal(err)
	}
	var subject string
	err = postgres.WithTx(e.t.Context(), e.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		subject, err = adapters.HandlePixSimEvent(ctx, tx, sqlOutbox{}, payload, e.clock.Now())
		return err
	})
	return subject, err //nolint:wrapcheck // the test reads the handler's error as it is.
}

// F5's done criterion: an expired quote does not take a payment as if on time, whether
// the job or the provider's event gets there first.
func TestPaidLateIsNotConfirmed(t *testing.T) {
	t.Run("job first", func(t *testing.T) {
		e := newExpiryEnv(t)
		id, attempt, ch := e.payment()
		e.at(expiresAt.Add(time.Second))
		e.pay(&ch)
		e.expire()
		got := e.state(id)
		if got.status != "expired" || !slices.Equal(got.events, []string{"payment.expired"}) || got.posted != 0 {
			t.Errorf("payment %+v; want expired with nothing posted", got)
		}
		if a, err := e.store.GetAttempt(t.Context(), attempt); err != nil || a.Status() != domain.AttemptAccepted {
			t.Errorf("attempt %s (%v); late money waits for review, the attempt is left as it was", a.Status(), err)
		}
	})
	t.Run("event first", func(t *testing.T) {
		e := newExpiryEnv(t)
		id, _, ch := e.payment()
		e.at(expiresAt.Add(time.Second))
		paid := e.pay(&ch)
		event := pixsim.Event{ID: "evt_1", Type: pixsim.ChargePaid, CreatedAt: *paid.PaidAt, Charge: paid}
		if _, err := e.handle(&event); !errors.Is(err, app.ErrEventDivergent) || !errors.Is(err, domain.ErrLatePayment) {
			t.Errorf("error = %v, want a late payment read as divergent", err)
		}
		if got := e.state(id); got.status != "pending" || got.posted != 0 || len(got.events) != 0 {
			t.Errorf("payment %+v; want it untouched until the job expires it", got)
		}
	})
}

// Decision 17's rule for expiry: no answer from the provider, no decision. The payment
// stays pending and the job asks again once the lease ends.
func TestProviderDownDoesNotExpire(t *testing.T) {
	e := newExpiryEnv(t)
	id, _, _ := e.payment()
	if err := e.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpGet, Kind: pixsim.FailBefore, Count: 1}); err != nil {
		t.Fatal(err)
	}
	e.at(expiresAt)
	if _, err := e.expirer.Expire(t.Context(), app.ExpiryBatch); err == nil {
		t.Error("Expire() hid the failed lookup")
	}
	if got := e.state(id); got.status != "pending" {
		t.Fatalf("payment %s after a failed lookup, want pending", got.status)
	}
	if n := e.expire(); n != 0 {
		t.Fatalf("claimed %d payments during the lease", n)
	}
	e.at(expiresAt.Add(app.ClaimLease))
	e.expire()
	if got := e.state(id); got.status != "expired" {
		t.Errorf("payment %s after the lease, want expired", got.status)
	}
}

// A rejected attempt cannot have been paid: nothing to ask.
func TestRejectedAttemptExpiresWithoutAsking(t *testing.T) {
	e := newExpiryEnv(t)
	id := newPayment(t, e.pool)
	a := create(t, e.store, id)
	apply(t, e.store, a.ID(), domain.Send, domain.Reject)
	e.at(expiresAt)
	e.expire()
	if got := e.state(id); got.status != "expired" || e.provider.gets.Load() != 0 {
		t.Errorf("payment %s after %d lookups, want expired after none", got.status, e.provider.gets.Load())
	}
}

func TestConcurrentExpirersExpireEachOnce(t *testing.T) {
	e := newExpiryEnv(t)
	const payments, expirers = 50, 4
	ids := make([]domain.PaymentID, payments)
	for i := range ids {
		ids[i], _, _ = e.payment()
	}
	e.at(expiresAt)
	var wg sync.WaitGroup
	for range expirers {
		wg.Go(func() {
			for {
				n, err := e.expirer.Expire(t.Context(), app.ExpiryBatch)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	for _, id := range ids {
		if got := e.state(id); got.status != "expired" || got.version != 2 || len(got.events) != 1 {
			t.Errorf("%s: %+v, want expired once", id, got)
		}
	}
	if n := e.provider.gets.Load(); n != payments {
		t.Errorf("%d lookups for %d payments, want one each", n, payments)
	}
}

// Expire re-checks under the payment's lock: the provider's event may confirm the
// payment while the job is asking, and a claim may run early. Neither is expired.
func TestExpireRechecksUnderTheLock(t *testing.T) {
	e := newExpiryEnv(t)
	early, _, _ := e.payment()
	if expired, err := e.store.Expire(t.Context(), early, expiresAt.Add(-time.Microsecond)); err != nil || expired {
		t.Errorf("Expire() before the quote expired = %v, %v; want false", expired, err)
	}

	paid, attempt, ch := e.payment()
	u := domain.AttemptUpdate{Event: domain.Succeed, ExternalID: ch.ID, BRCode: ch.BRCode, PaidAt: t0.Add(time.Minute)}
	if _, _, err := e.store.ApplyAttempt(t.Context(), attempt, u, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if expired, err := e.store.Expire(t.Context(), paid, expiresAt); err != nil || expired {
		t.Errorf("Expire() of a paid payment = %v, %v; want false", expired, err)
	}
	if got := e.state(paid); got.status != "paid" || got.version != 2 || len(got.events) != 1 {
		t.Errorf("paid payment %+v after Expire, want it untouched", got)
	}
}

// SKIP LOCKED: a payment another transaction holds, such as one being confirmed, is
// passed over, not waited on.
func TestALockedPaymentDoesNotBlockTheExpiry(t *testing.T) {
	e := newExpiryEnv(t)
	held, _, _ := e.payment()
	free, _, _ := e.payment()
	tx, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := tx.Exec(t.Context(), "SELECT 1 FROM payments WHERE id = $1 FOR NO KEY UPDATE", held); err != nil {
		t.Fatal(err)
	}

	e.at(expiresAt)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if n, err := e.expirer.Expire(ctx, app.ExpiryBatch); err != nil || n != 1 {
		t.Fatalf("Expire() = %d, %v while one payment was locked; want the other one at once", n, err)
	}
	if got := e.state(free); got.status != "expired" {
		t.Errorf("free payment %s, want expired", got.status)
	}
}
