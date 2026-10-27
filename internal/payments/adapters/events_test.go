package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

const paidCode = "00020101021126580014br.gov.bcb.pix0136123e4567-e12b-12d1-a456-4266554400005204000053039865406101.505802BR5916CROSS BORDER PAY6009SAO PAULO62290525ABCDEFGHIJKLMNOPQRSTUVWXY6304ABCD"

type eventEnv struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *adapters.Postgres
}

func newEventEnv(t *testing.T) *eventEnv {
	t.Helper()
	pool := pgtest.NewPool(t)
	return &eventEnv{t: t, pool: pool, store: adapters.NewPostgres(pool)}
}

// attempt stores a payment with an attempt moved by events, and returns the amount its
// quote asks the payer for.
func (e *eventEnv) attempt(events ...domain.AttemptEvent) (domain.AttemptID, money.Money) {
	e.t.Helper()
	payment := newPayment(e.t, e.pool)
	a := create(e.t, e.store, payment)
	apply(e.t, e.store, a.ID(), events...)
	var amount int64
	err := e.pool.QueryRow(e.t.Context(),
		"SELECT q.payer_amount FROM payments p JOIN quotes q ON q.id = p.quote_id WHERE p.id = $1", payment).Scan(&amount)
	if err != nil {
		e.t.Fatal(err)
	}
	return a.ID(), money.New(amount, money.BRL)
}

func paid(key string, amount money.Money) pixsim.Event {
	paidAt := t0.Add(time.Minute)
	return pixsim.Event{ID: "evt_1", Type: pixsim.ChargePaid, CreatedAt: paidAt, Charge: pixsim.Charge{
		ID: "chg_1", IdempotencyKey: key, Status: pixsim.Paid, Amount: amount, BRCode: paidCode,
		ExpiresAt: t0.Add(domain.PixQuoteValidity), CreatedAt: t0, PaidAt: &paidAt,
	}}
}

// handle runs the handler in a transaction it commits whatever happens, as the inbox
// does when recording an outcome.
func (e *eventEnv) handle(ctx context.Context, event *pixsim.Event) (string, error) {
	e.t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		e.t.Fatal(err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	subject, err := adapters.HandlePixSimEvent(ctx, tx, payload, t0.Add(2*time.Minute))
	if cerr := tx.Commit(ctx); cerr != nil && err == nil {
		e.t.Fatal(cerr)
	}
	return subject, err
}

func (e *eventEnv) status(id domain.AttemptID) domain.PaymentAttempt {
	e.t.Helper()
	a, err := e.store.GetAttempt(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func TestChargePaidSucceedsTheAttempt(t *testing.T) {
	for _, from := range [][]domain.AttemptEvent{
		{domain.Send, domain.Accept},
		{domain.Send, domain.Ambiguous},
	} {
		e := newEventEnv(t)
		id, amount := e.attempt(from...)
		event := paid(id.String(), amount)
		subject, err := e.handle(t.Context(), &event)
		a := e.status(id)
		if err != nil || subject != id.String() || a.Status() != domain.AttemptSucceeded || a.ExternalID() != "chg_1" || a.BRCode() != paidCode {
			t.Fatalf("from %v: subject %q, %v; attempt %s %q; want succeeded with the charge", from, subject, err, a.Status(), a.ExternalID())
		}
		var next *time.Time
		if err := e.pool.QueryRow(t.Context(), "SELECT next_check_at FROM payment_attempts WHERE id = $1", id).Scan(&next); err != nil || next != nil {
			t.Errorf("from %v: still scheduled at %v (%v); a paid attempt leaves the resolver's queue", from, next, err)
		}
	}
}

// Already reflected: the resolver saw the payment first, or the event came twice.
func TestChargePaidTwiceIsANoOp(t *testing.T) {
	e := newEventEnv(t)
	id, amount := e.attempt(domain.Send, domain.Accept)
	event := paid(id.String(), amount)
	if _, err := e.handle(t.Context(), &event); err != nil {
		t.Fatal(err)
	}
	first := e.status(id)
	subject, err := e.handle(t.Context(), &event)
	if second := e.status(id); err != nil || subject != id.String() || !second.UpdatedAt().Equal(first.UpdatedAt()) {
		t.Errorf("second event: %q, %v; want a no-op for %s", subject, err, id)
	}
}

func TestChargePaidForAnAttemptNotStoredYet(t *testing.T) {
	e := newEventEnv(t)
	missing := domain.AttemptID{1}
	event := paid(missing.String(), money.New(10150, money.BRL))
	if _, err := e.handle(t.Context(), &event); !errors.Is(err, app.ErrAttemptNotFound) {
		t.Errorf("error = %v, want %v", err, app.ErrAttemptNotFound)
	}
}

func TestDivergentEventsChangeNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		from  []domain.AttemptEvent
		event func(id domain.AttemptID, amount money.Money) pixsim.Event
	}{
		"other amount": {[]domain.AttemptEvent{domain.Send, domain.Accept}, func(id domain.AttemptID, amount money.Money) pixsim.Event {
			return paid(id.String(), money.New(amount.Amount()+1, money.BRL))
		}},
		"other currency": {[]domain.AttemptEvent{domain.Send, domain.Accept}, func(id domain.AttemptID, amount money.Money) pixsim.Event {
			return paid(id.String(), money.New(amount.Amount(), money.USD))
		}},
		"rejected attempt": {[]domain.AttemptEvent{domain.Send, domain.Reject}, func(id domain.AttemptID, amount money.Money) pixsim.Event {
			return paid(id.String(), amount)
		}},
		"never sent": {nil, func(id domain.AttemptID, amount money.Money) pixsim.Event {
			return paid(id.String(), amount)
		}},
		"not our key": {[]domain.AttemptEvent{domain.Send, domain.Accept}, func(_ domain.AttemptID, amount money.Money) pixsim.Event {
			return paid("order-123", amount)
		}},
		"charge not paid": {[]domain.AttemptEvent{domain.Send, domain.Accept}, func(id domain.AttemptID, amount money.Money) pixsim.Event {
			ev := paid(id.String(), amount)
			ev.Charge.Status = pixsim.Active
			return ev
		}},
	} {
		e := newEventEnv(t)
		id, amount := e.attempt(tc.from...)
		before := e.status(id)
		event := tc.event(id, amount)
		if _, err := e.handle(t.Context(), &event); !errors.Is(err, app.ErrEventDivergent) {
			t.Errorf("%s: error = %v, want %v", name, err, app.ErrEventDivergent)
		}
		if after := e.status(id); after.Status() != before.Status() || after.ExternalID() != before.ExternalID() {
			t.Errorf("%s: attempt went from %s to %s", name, before.Status(), after.Status())
		}
	}
}

// The provider naming the attempt with another charge ID is a contradiction, not news.
func TestChargePaidWithAnotherChargeIDDiverges(t *testing.T) {
	e := newEventEnv(t)
	id, amount := e.attempt(domain.Send)
	if _, _, err := e.store.ApplyAttempt(t.Context(), id, domain.AttemptUpdate{Event: domain.Accept, ExternalID: "chg_other"}, t0); err != nil {
		t.Fatal(err)
	}
	event := paid(id.String(), amount)
	_, err := e.handle(t.Context(), &event)
	if a := e.status(id); !errors.Is(err, app.ErrEventDivergent) || a.Status() != domain.AttemptAccepted {
		t.Errorf("error = %v, status %s; want divergent and still accepted", err, a.Status())
	}
}

func TestUnknownEventTypeIsNotHandled(t *testing.T) {
	e := newEventEnv(t)
	id, amount := e.attempt(domain.Send, domain.Accept)
	event := paid(id.String(), amount)
	event.Type = "charge.refunded"
	if _, err := e.handle(t.Context(), &event); !errors.Is(err, app.ErrUnknownEventType) {
		t.Errorf("error = %v, want %v", err, app.ErrUnknownEventType)
	}
}

// Section 4.4: events of one payment are applied one at a time, behind its row lock.
func TestChargePaidWaitsForThePaymentLock(t *testing.T) {
	e := newEventEnv(t)
	id, amount := e.attempt(domain.Send, domain.Accept)
	tx, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := tx.Exec(t.Context(), "SELECT 1 FROM payments FOR UPDATE"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	event := paid(id.String(), amount)
	if _, err := e.handle(ctx, &event); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v while another transaction held the payment, want it to wait", err)
	}
}

// FOR KEY SHARE is what a foreign key check takes on the payment while a row pointing at
// it is inserted, such as a new attempt; the event's lock must not wait for it.
func TestChargePaidDoesNotWaitForAForeignKeyCheck(t *testing.T) {
	e := newEventEnv(t)
	id, amount := e.attempt(domain.Send, domain.Accept)
	tx, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := tx.Exec(t.Context(), "SELECT 1 FROM payments FOR KEY SHARE"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	event := paid(id.String(), amount)
	if _, err := e.handle(ctx, &event); err != nil {
		t.Errorf("error = %v while another transaction held only a key share", err)
	}
}
