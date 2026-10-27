package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	eventsadapters "github.com/gengibrepower/cross-border-pay/internal/events/adapters"
	eventsapp "github.com/gengibrepower/cross-border-pay/internal/events/app"
	events "github.com/gengibrepower/cross-border-pay/internal/events/domain"
	fxadapters "github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
	pixsimserver "github.com/gengibrepower/cross-border-pay/internal/providers/pixsim/server"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

type inboxEnv struct {
	t         *testing.T
	pool      *pgxpool.Pool
	clock     *clock.Fake
	store     *eventsadapters.Postgres
	payments  *paymentsadapters.Postgres
	processor *eventsapp.Processor
}

func newInboxEnv(t *testing.T) *inboxEnv {
	t.Helper()
	pool := pgtest.NewPool(t)
	c := clock.NewFake(t0)
	store := eventsadapters.NewPostgres(pool)
	return &inboxEnv{
		t: t, pool: pool, clock: c, store: store, payments: paymentsadapters.NewPostgres(pool),
		processor: eventsapp.NewProcessor(store, paymentEvents{clock: c}, c, slog.New(slog.DiscardHandler)),
	}
}

// attempt stores a payment of 20 USD with an attempt in created and returns what the
// payer must pay.
func (e *inboxEnv) attempt() (payments.AttemptID, money.Money) {
	e.t.Helper()
	ctx := e.t.Context()
	var merchant [16]byte
	if err := e.pool.QueryRow(ctx, "INSERT INTO merchants (name) VALUES ('m') RETURNING id").Scan(&merchant); err != nil {
		e.t.Fatal(err)
	}
	rate, err := fx.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		e.t.Fatal(err)
	}
	q, err := fx.NewQuote(money.New(2000, money.USD), rate, fx.DefaultPricing, t0, t0.Add(payments.PixQuoteValidity))
	if err != nil {
		e.t.Fatal(err)
	}
	quote, err := fxadapters.SaveQuote(ctx, e.pool, &q)
	if err != nil {
		e.t.Fatal(err)
	}
	var payment payments.PaymentID
	err = e.pool.QueryRow(ctx, `INSERT INTO payments (merchant_id, quote_id, status, method, amount, currency, reference, created_at, updated_at)
		VALUES ($1, $2, 'pending', 'pix', 2000, 'USD', 'order-1', $3, $3) RETURNING id`, merchant, quote, t0).Scan(&payment)
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.payments.CreateAttempt(ctx, payment, paymentsadapters.PixSimName, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	return a.ID(), q.PayerAmount()
}

func (e *inboxEnv) apply(id payments.AttemptID, u payments.AttemptUpdate) {
	e.t.Helper()
	if _, _, err := e.payments.ApplyAttempt(e.t.Context(), id, u, t0); err != nil {
		e.t.Fatal(err)
	}
}

// paidBySimulator creates the charge at a real simulator, pays it and returns the
// charge.paid body the simulator delivered.
func (e *inboxEnv) paidBySimulator(id payments.AttemptID, amount money.Money) (charge pixsim.Charge, payload []byte) {
	e.t.Helper()
	delivered := make(chan []byte, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		delivered <- body
	}))
	e.t.Cleanup(hook.Close)
	sim, err := pixsimserver.New(&pixsimserver.Config{
		Seed: 1, PixKey: "123e4567-e12b-12d1-a456-426655440000", MerchantName: "CROSS BORDER PAY", MerchantCity: "SAO PAULO",
		WebhookURL: hook.URL, WebhookSecret: []byte("s3cret"),
	}, e.clock, slog.New(slog.DiscardHandler))
	if err != nil {
		e.t.Fatal(err)
	}
	mux := http.NewServeMux()
	sim.Register(mux)
	srv := httptest.NewServer(mux)
	e.t.Cleanup(srv.Close)
	client, err := pixsim.NewClient(srv.URL, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	charge, err = client.Create(e.t.Context(), id.String(), pixsim.CreateCharge{Amount: amount, ExpiresAt: t0.Add(payments.PixQuoteValidity)})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := client.Pay(e.t.Context(), charge.ID); err != nil {
		e.t.Fatal(err)
	}
	return charge, <-delivered
}

func (e *inboxEnv) receive(provider string, payload []byte) {
	e.t.Helper()
	var head struct{ ID, Type string }
	if err := json.Unmarshal(payload, &head); err != nil {
		e.t.Fatal(err)
	}
	in := events.Incoming{Provider: provider, ExternalID: head.ID, Type: head.Type, Payload: payload, Signature: "checked by the API"}
	if _, _, err := e.store.Insert(e.t.Context(), &in, e.clock.Now()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *inboxEnv) event() (status, subject string) {
	e.t.Helper()
	var s *string
	if err := e.pool.QueryRow(e.t.Context(), "SELECT status, subject_id FROM inbox_events").Scan(&status, &s); err != nil {
		e.t.Fatal(err)
	}
	if s != nil {
		subject = *s
	}
	return status, subject
}

func (e *inboxEnv) process() {
	e.t.Helper()
	if _, err := e.processor.Process(e.t.Context(), eventsapp.ProcessBatch); err != nil {
		e.t.Fatalf("Process() error = %v", err)
	}
}

// The whole return path behind the API: the simulator's own charge.paid, stored in the
// inbox, applied by the worker's processor to the attempt it names.
func TestWorkerAppliesASimulatorPayment(t *testing.T) {
	e := newInboxEnv(t)
	id, amount := e.attempt()
	charge, payload := e.paidBySimulator(id, amount)
	e.apply(id, payments.AttemptUpdate{Event: payments.Send})
	e.apply(id, payments.AttemptUpdate{Event: payments.Accept, ExternalID: charge.ID, BRCode: charge.BRCode})

	e.receive("pixsim", payload)
	e.process()

	a, err := e.payments.GetAttempt(t.Context(), id)
	if status, subject := e.event(); err != nil || a.Status() != payments.AttemptSucceeded || status != "processed" || subject != id.String() {
		t.Errorf("attempt %s (%v), event %s for %q; want succeeded and processed for %s", a.Status(), err, status, subject, id)
	}
}

func TestWorkerReadsPaymentsErrorsAsOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		edit     func(e *pixsim.Event)
		status   string
	}{
		{"attempt not stored yet", "pixsim", func(e *pixsim.Event) { e.Charge.IdempotencyKey = payments.AttemptID{1}.String() }, "pending"},
		{"another amount", "pixsim", func(e *pixsim.Event) { e.Charge.Amount = money.New(1, money.BRL) }, "failed"},
		{"unhandled type", "pixsim", func(e *pixsim.Event) { e.Type = "charge.refunded" }, "ignored"},
		{"no handler for the provider", "acme", func(*pixsim.Event) {}, "ignored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInboxEnv(t)
			id, amount := e.attempt()
			_, payload := e.paidBySimulator(id, amount)
			e.apply(id, payments.AttemptUpdate{Event: payments.Send})
			var event pixsim.Event
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatal(err)
			}
			tc.edit(&event)
			edited, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			e.receive(tc.provider, edited)
			e.process()
			if status, _ := e.event(); status != tc.status {
				t.Errorf("event %s, want %s", status, tc.status)
			}
		})
	}
}
