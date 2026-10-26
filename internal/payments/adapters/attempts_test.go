package adapters_test

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	fxadapters "github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

const pixsim domain.ProviderName = "pixsim"

func newPayment(t *testing.T, pool *pgxpool.Pool) domain.PaymentID {
	t.Helper()
	ctx := t.Context()
	var merchant [16]byte
	if err := pool.QueryRow(ctx, "INSERT INTO merchants (name) VALUES ('m') RETURNING id").Scan(&merchant); err != nil {
		t.Fatal(err)
	}
	rate, err := fx.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		t.Fatal(err)
	}
	q, err := fx.NewQuote(money.New(2000, money.USD), rate, fx.DefaultPricing, t0, t0.Add(domain.PixQuoteValidity))
	if err != nil {
		t.Fatal(err)
	}
	quote, err := fxadapters.SaveQuote(ctx, pool, &q)
	if err != nil {
		t.Fatal(err)
	}
	var id [16]byte
	err = pool.QueryRow(ctx, `INSERT INTO payments (merchant_id, quote_id, status, method, amount, currency, reference, created_at, updated_at)
		VALUES ($1, $2, 'pending', 'pix', 2000, 'USD', 'order-1', $3, $3) RETURNING id`, merchant, quote, t0).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func create(t *testing.T, store *adapters.Postgres, payment domain.PaymentID) domain.PaymentAttempt {
	t.Helper()
	a, err := store.CreateAttempt(t.Context(), payment, pixsim, t0)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func apply(t *testing.T, store *adapters.Postgres, id domain.AttemptID, events ...domain.AttemptEvent) {
	t.Helper()
	for _, e := range events {
		if _, _, err := store.ApplyAttempt(t.Context(), id, e, "", t0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCreateAttemptStoresItCreated(t *testing.T) {
	pool := pgtest.NewPool(t)
	store := adapters.NewPostgres(pool)
	payment := newPayment(t, pool)

	created := create(t, store, payment)
	if created.Status() != domain.AttemptCreated || created.Payment() != payment || created.Provider() != pixsim ||
		created.ExternalID() != "" || !created.CreatedAt().Equal(t0) || !created.UpdatedAt().Equal(t0) {
		t.Fatalf("CreateAttempt() = %+v", created)
	}
	got, err := store.GetAttempt(t.Context(), created.ID())
	if err != nil || got != created {
		t.Errorf("GetAttempt() = %+v, %v; want %+v", got, err, created)
	}
}

func TestUnknownAttempt(t *testing.T) {
	store := adapters.NewPostgres(pgtest.NewPool(t))
	if _, err := store.GetAttempt(t.Context(), domain.AttemptID{1}); !errors.Is(err, app.ErrAttemptNotFound) {
		t.Errorf("GetAttempt() error = %v, want %v", err, app.ErrAttemptNotFound)
	}
	if _, _, err := store.ApplyAttempt(t.Context(), domain.AttemptID{1}, domain.Send, "", t0); !errors.Is(err, app.ErrAttemptNotFound) {
		t.Errorf("ApplyAttempt() error = %v, want %v", err, app.ErrAttemptNotFound)
	}
}

func TestApplyAttemptStoresTheTransition(t *testing.T) {
	pool := pgtest.NewPool(t)
	store := adapters.NewPostgres(pool)
	a := create(t, store, newPayment(t, pool))
	apply(t, store, a.ID(), domain.Send)

	at := t0.Add(time.Minute)
	moved, changed, err := store.ApplyAttempt(t.Context(), a.ID(), domain.Accept, "ext-1", at)
	if err != nil || !changed {
		t.Fatalf("ApplyAttempt(accept) = %v, %v; want a change", changed, err)
	}
	got, err := store.GetAttempt(t.Context(), a.ID())
	if err != nil || got != moved || got.Status() != domain.AttemptAccepted || got.ExternalID() != "ext-1" || !got.UpdatedAt().Equal(at) {
		t.Fatalf("stored attempt = %+v, %v; want accepted ext-1 at %v", got, err, at)
	}

	_, changed, err = store.ApplyAttempt(t.Context(), a.ID(), domain.Accept, "ext-1", at.Add(time.Minute))
	if err != nil || changed {
		t.Errorf("duplicate accept = %v, %v; want a no-op", changed, err)
	}
	_, _, err = store.ApplyAttempt(t.Context(), a.ID(), domain.Reject, "", at.Add(time.Minute))
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("reject on accepted error = %v, want %v", err, domain.ErrInvalidTransition)
	}
	if after, _ := store.GetAttempt(t.Context(), a.ID()); after != got {
		t.Errorf("a no-op or a refused event changed the row: %+v, want %+v", after, got)
	}
}

func TestOneLiveAttemptPerPayment(t *testing.T) {
	pool := pgtest.NewPool(t)
	store := adapters.NewPostgres(pool)
	payment := newPayment(t, pool)

	first := create(t, store, payment)
	if _, err := store.CreateAttempt(t.Context(), payment, pixsim, t0); !errors.Is(err, app.ErrLiveAttempt) {
		t.Fatalf("second attempt while the first is created: error = %v, want %v", err, app.ErrLiveAttempt)
	}

	apply(t, store, first.ID(), domain.Send, domain.Reject)
	second := create(t, store, payment)

	apply(t, store, second.ID(), domain.Send, domain.Succeed)
	if _, err := store.CreateAttempt(t.Context(), payment, pixsim, t0); !errors.Is(err, app.ErrLiveAttempt) {
		t.Errorf("attempt after a success: error = %v, want %v", err, app.ErrLiveAttempt)
	}
}

func TestConcurrentCreatesLeaveOneAttempt(t *testing.T) {
	pool := pgtest.NewPool(t)
	store := adapters.NewPostgres(pool)
	payment := newPayment(t, pool)

	const workers = 20
	errs := make([]error, workers)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range workers {
		done.Go(func() {
			start.Wait()
			_, errs[i] = store.CreateAttempt(t.Context(), payment, pixsim, t0)
		})
	}
	start.Done()
	done.Wait()

	created := 0
	for _, err := range errs {
		switch {
		case err == nil:
			created++
		case !errors.Is(err, app.ErrLiveAttempt):
			t.Errorf("CreateAttempt() error = %v, want nil or %v", err, app.ErrLiveAttempt)
		}
	}
	if created != 1 {
		t.Errorf("%d of %d concurrent creates succeeded, want 1", created, workers)
	}
}

// Accept and reject racing from sent: the row lock lets one win, and every loser sees
// the winner's status, so it gets a duplicate or an invalid transition, never a second
// change.
func TestConcurrentEventsHaveOneWinner(t *testing.T) {
	pool := pgtest.NewPool(t)
	store := adapters.NewPostgres(pool)

	const rounds, workers = 25, 8
	for range rounds {
		a := create(t, store, newPayment(t, pool))
		apply(t, store, a.ID(), domain.Send)

		type result struct {
			event   domain.AttemptEvent
			changed bool
			err     error
		}
		results := make([]result, workers)
		var start, done sync.WaitGroup
		start.Add(1)
		for i := range workers {
			e := domain.Accept
			if i%2 == 1 {
				e = domain.Reject
			}
			done.Go(func() {
				start.Wait()
				_, changed, err := store.ApplyAttempt(t.Context(), a.ID(), e, "", t0)
				results[i] = result{e, changed, err}
			})
		}
		start.Done()
		done.Wait()

		stored, err := store.GetAttempt(t.Context(), a.ID())
		if err != nil {
			t.Fatal(err)
		}
		winner := domain.Accept
		if stored.Status() == domain.AttemptRejected {
			winner = domain.Reject
		}
		winners := 0
		for _, r := range results {
			switch {
			case r.changed:
				winners++
				if r.event != winner {
					t.Errorf("%s changed the attempt but it ended %s", r.event, stored.Status())
				}
			case r.event == winner:
				if r.err != nil {
					t.Errorf("duplicate %s error = %v, want a no-op", r.event, r.err)
				}
			case !errors.Is(r.err, domain.ErrInvalidTransition):
				t.Errorf("losing %s error = %v, want %v", r.event, r.err, domain.ErrInvalidTransition)
			}
		}
		if winners != 1 {
			t.Fatalf("%d events changed the attempt, want 1", winners)
		}
	}
}
