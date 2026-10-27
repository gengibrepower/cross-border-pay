package adapters_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// heldState is the money confirmed by the provider that confirmed nothing.
type heldState struct {
	kinds []string // review cases, oldest first
	held  int64    // on divergence:BRL
	// Cases whose posting is missing, and currencies whose entries do not sum to zero:
	// both always empty.
	unposted   int
	unbalanced []string
}

func (h heldState) equal(o heldState) bool {
	return slices.Equal(h.kinds, o.kinds) && h.held == o.held && h.unposted == o.unposted &&
		slices.Equal(h.unbalanced, o.unbalanced)
}

func heldFor(t *testing.T, pool *pgxpool.Pool) heldState {
	t.Helper()
	var h heldState
	err := pool.QueryRow(t.Context(), `SELECT
		coalesce((SELECT array_agg(kind ORDER BY created_at, id) FROM review_cases), '{}'),
		coalesce((SELECT -sum(en.amount) FROM ledger_entries AS en JOIN ledger_accounts AS a ON a.id = en.account_id
			WHERE a.code = 'divergence:BRL'), 0)::bigint,
		(SELECT count(*) FROM review_cases AS c
			WHERE NOT EXISTS (SELECT 1 FROM ledger_transactions AS lt WHERE lt.reference = c.ledger_reference)),
		coalesce((SELECT array_agg(currency ORDER BY currency) FROM
			(SELECT currency FROM ledger_entries GROUP BY currency HAVING sum(amount) <> 0) AS u), '{}')`).
		Scan(&h.kinds, &h.held, &h.unposted, &h.unbalanced)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Section 4.4 for an amount the quote did not ask for, by the event and by the
// resolver: the second one used to confirm the payment without comparing amounts.
func TestOtherAmountIsHeldForReview(t *testing.T) {
	for name, report := range map[string]func(*eventEnv, domain.AttemptID, money.Money) error{
		"event": func(e *eventEnv, id domain.AttemptID, amount money.Money) error {
			event := paid(id.String(), amount)
			_, err := e.handle(t.Context(), &event)
			return err
		},
		"resolver": func(e *eventEnv, id domain.AttemptID, amount money.Money) error {
			u := domain.AttemptUpdate{Event: domain.Succeed, ExternalID: "chg_1", BRCode: paidCode, PaidAt: t0.Add(time.Minute), Amount: amount}
			_, _, err := e.store.ApplyAttempt(t.Context(), id, &u, t0.Add(2*time.Minute))
			return err
		},
	} {
		e := newEventEnv(t)
		id, quoted := e.attempt(domain.Send, domain.Accept)
		short := money.New(quoted.Amount()-1, money.BRL)
		if err := report(e, id, short); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if status, a := e.paymentStatus(id), e.status(id); status != "pending" || a.Status() != domain.AttemptSucceeded {
			t.Errorf("%s: payment %s, attempt %s; want pending and succeeded", name, status, a.Status())
		}
		want := heldState{kinds: []string{"amount_mismatch"}, held: short.Amount()}
		if h := heldFor(t, e.pool); !h.equal(want) {
			t.Errorf("%s: held %+v, want %+v", name, h, want)
		}
	}
}

// Money for an attempt the provider had rejected, or under another charge than the one
// the attempt has: the attempt keeps what it knew, and the money is held.
func TestMoneyTheAttemptCannotTakeIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name   string
		from   []domain.AttemptEvent
		charge string
		kind   string
		status domain.AttemptStatus
	}{
		{"after a rejection", []domain.AttemptEvent{domain.Send, domain.Reject}, "chg_1", "paid_after_rejection", domain.AttemptRejected},
		{"another charge", []domain.AttemptEvent{domain.Send, domain.Accept}, "chg_2", "second_charge", domain.AttemptAccepted},
	} {
		e := newEventEnv(t)
		id, amount := e.attempt(tc.from...)
		if tc.status == domain.AttemptAccepted {
			// e.attempt accepts without a charge; this one has chg_1.
			if _, _, err := e.store.ApplyAttempt(t.Context(), id, &domain.AttemptUpdate{Event: domain.Accept, ExternalID: "chg_1"}, t0); err != nil {
				t.Fatal(err)
			}
		}
		event := paid(id.String(), amount)
		event.Charge.ID = tc.charge
		if _, err := e.handle(t.Context(), &event); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if a := e.status(id); a.Status() != tc.status || e.paymentStatus(id) != "pending" {
			t.Errorf("%s: attempt %s, payment %s; want %s and pending", tc.name, a.Status(), e.paymentStatus(id), tc.status)
		}
		want := heldState{kinds: []string{tc.kind}, held: amount.Amount()}
		if h := heldFor(t, e.pool); !h.equal(want) {
			t.Errorf("%s: held %+v, want %+v", tc.name, h, want)
		}
	}
}

// The same divergent charge reported again, by a redelivered event or by the resolver,
// finds its case open and posts nothing more.
func TestADivergentChargeIsHeldOnce(t *testing.T) {
	e := newEventEnv(t)
	id, quoted := e.attempt(domain.Send, domain.Reject)
	event := paid(id.String(), quoted)
	const reports = 20
	var wg sync.WaitGroup
	for range reports {
		wg.Go(func() {
			if _, err := e.handle(t.Context(), &event); err != nil {
				t.Errorf("handle: %v", err)
			}
		})
	}
	wg.Wait()
	want := heldState{kinds: []string{"paid_after_rejection"}, held: quoted.Amount()}
	if h := heldFor(t, e.pool); !h.equal(want) {
		t.Errorf("held %+v after %d reports, want %+v", h, reports, want)
	}
}

type failingReviews struct{}

func (failingReviews) Open(context.Context, pgx.Tx, *domain.ReviewCase) (bool, error) {
	return false, errors.New("review queue down")
}

// The case commits with the posting and the attempt, or none of them does.
func TestAReviewQueueDownHoldsNothing(t *testing.T) {
	e := newEventEnv(t)
	id, quoted := e.attempt(domain.Send, domain.Accept)
	broken := adapters.NewPostgres(e.pool, adapters.Sinks{Outbox: sqlOutbox{}, Reviews: failingReviews{}})
	u := domain.AttemptUpdate{Event: domain.Succeed, ExternalID: "chg_1", BRCode: paidCode, PaidAt: t0.Add(time.Hour), Amount: quoted}
	if _, _, err := broken.ApplyAttempt(t.Context(), id, &u, t0.Add(time.Hour)); err == nil {
		t.Fatal("held money without a review case")
	}
	if a := e.status(id); a.Status() != domain.AttemptAccepted {
		t.Errorf("attempt %s, want accepted", a.Status())
	}
	if h := heldFor(t, e.pool); !h.equal(heldState{}) {
		t.Errorf("held %+v, want nothing", h)
	}
}

// An attempt that already succeeded and only learns its BR Code changes, but it is not
// paid again: the payment is not confirmed twice, nor held as a second charge.
func TestLearningTheBRCodeIsNotASecondPayment(t *testing.T) {
	e := newEventEnv(t)
	id, quoted := e.attempt(domain.Send, domain.Accept)
	u := domain.AttemptUpdate{Event: domain.Succeed, ExternalID: "chg_1", PaidAt: t0.Add(time.Minute), Amount: quoted}
	if _, _, err := e.store.ApplyAttempt(t.Context(), id, &u, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	event := paid(id.String(), quoted)
	if _, err := e.handle(t.Context(), &event); err != nil {
		t.Fatal(err)
	}
	if a := e.status(id); a.BRCode() != paidCode || e.paymentStatus(id) != "paid" || e.confirmations(id) != 1 {
		t.Errorf("BR Code %q, payment %s, %d confirmations; want the code learned, paid once",
			a.BRCode(), e.paymentStatus(id), e.confirmations(id))
	}
	if h := heldFor(t, e.pool); !h.equal(heldState{}) {
		t.Errorf("held %+v, want nothing", h)
	}
}
