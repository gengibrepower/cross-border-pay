package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gengibrepower/cross-border-pay/internal/boleto"
	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	eventsadapters "github.com/gengibrepower/cross-border-pay/internal/events/adapters"
	eventsapp "github.com/gengibrepower/cross-border-pay/internal/events/app"
	events "github.com/gengibrepower/cross-border-pay/internal/events/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/boletosim"
)

const payerCPF = "52998224725"

func boletoBody(reference, payer string) string {
	return fmt.Sprintf(`{"amount": {"value": 2000, "currency": "USD"}, "method": "boleto", "reference": %q, "payer": %s}`, reference, payer)
}

var maria = `{"name": "Maria Silva", "document": "529.982.247-25", "email": "maria@example.com"}`

type boletoPayment struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Quote  struct {
		PayerAmount struct{ Value int64 } `json:"payer_amount"`
		ExpiresAt   time.Time             `json:"expires_at"`
	} `json:"quote"`
	Boleto *struct {
		Status        string `json:"status"`
		DueDate       string `json:"due_date"`
		Barcode       string `json:"barcode"`
		DigitableLine string `json:"digitable_line"`
	} `json:"boleto"`
	Pix  *json.RawMessage `json:"pix"`
	Card *json.RawMessage `json:"card"`
}

func boletoOf(t *testing.T, rec *httptest.ResponseRecorder) boletoPayment {
	t.Helper()
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var p boletoPayment
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Boleto == nil || p.Pix != nil || p.Card != nil {
		t.Fatalf("a boleto payment must carry boleto and nothing else: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), payerCPF) || strings.Contains(rec.Body.String(), "529.982.247-25") {
		t.Fatalf("the response shows the payer's CPF: %s", rec.Body)
	}
	return p
}

// processInbox runs the worker's step for the events the simulator delivered, with
// the same handler the worker gives the boleto simulator's events.
func (c *contract) processInbox() {
	c.t.Helper()
	p := eventsapp.NewProcessor(eventsadapters.NewPostgres(c.pool), boletoEvents{c: c}, c.clock, slog.New(slog.DiscardHandler))
	if _, err := p.Process(c.t.Context(), 10); err != nil {
		c.t.Fatal(err)
	}
}

type boletoEvents struct{ c *contract }

func (h boletoEvents) Handle(ctx context.Context, tx pgx.Tx, e *events.Event) (eventsapp.Result, error) {
	sinks := paymentSinks()
	subject, err := paymentsadapters.HandleBoletoSimEvent(ctx, tx, &sinks, e.Payload, h.c.clock.Now())
	if err != nil {
		return eventsapp.Result{Subject: subject, Outcome: events.Failed, Reason: err.Error()}, nil
	}
	return eventsapp.Result{Subject: subject, Outcome: events.Processed}, nil
}

// t0 is Sunday 1 November 2026, 09:00 in São Paulo: due on Wednesday 4, three days
// later, and received until the end of it (decision 44).
func TestBoletoPaymentReturnsAPayableBoleto(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	rec := c.do(postPayment(t, key, "order-125", boletoBody("order-125", maria)), true)
	p := boletoOf(t, rec)

	if p.Status != "pending" || p.Boleto.Status != "ready" || p.Boleto.DueDate != "2026-11-04" {
		t.Fatalf("payment = %+v, boleto %+v; want pending, ready and due 2026-11-04", p, *p.Boleto)
	}
	if want := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC); !p.Quote.ExpiresAt.Equal(want) {
		t.Errorf("quote expires %s, want %s, the end of the due date in São Paulo", p.Quote.ExpiresAt, want)
	}
	parsed, err := boleto.Parse(p.Boleto.Barcode, calendar.On(t0, time.UTC))
	if err != nil || parsed.Amount != money.New(p.Quote.PayerAmount.Value, money.BRL) || parsed.Due.String() != "2026-11-04" || parsed.Bank != "999" {
		t.Errorf("barcode %s says %+v, %v; want the quote's amount, due 2026-11-04", p.Boleto.Barcode, parsed, err)
	}
	if back, err := boleto.BarcodeFromLine(p.Boleto.DigitableLine); err != nil || back != p.Boleto.Barcode {
		t.Errorf("linha digitável %q is barcode %q, %v; want %q", p.Boleto.DigitableLine, back, err, p.Boleto.Barcode)
	}
	if got := c.do(getPayment(t, key, p.ID), true); !bytes.Equal(got.Body.Bytes(), rec.Body.Bytes()) {
		t.Errorf("GET differs from the POST:\n%s\n%s", got.Body, rec.Body)
	}

	// The bank has the boleto under the attempt, registered to the payer it was given.
	a := c.attemptOf(p.ID)
	at, err := c.boleto.Get(t.Context(), a.id.String())
	if err != nil || at.ID != a.externalID || at.Barcode != p.Boleto.Barcode || a.status != "accepted" {
		t.Errorf("attempt %+v, boleto %+v, %v", a, at, err)
	}
	var name, doc string
	if err := c.pool.QueryRow(t.Context(), "SELECT payer_name, payer_document FROM payment_attempts WHERE id = $1", a.id).Scan(&name, &doc); err != nil ||
		name != "Maria Silva" || doc != payerCPF {
		t.Errorf("stored payer %q %q, %v", name, doc, err)
	}
}

// The payer pays at the bank, boleto.paid arrives at the real route, and the worker's
// step confirms the payment with the books of any other method.
func TestPaidBoletoConfirmsThePayment(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	p := boletoOf(t, c.do(postPayment(t, key, "order-125", boletoBody("order-125", maria)), true))

	c.clock.Advance(26 * time.Hour) // Monday, before the due date
	if _, err := c.boleto.Pay(t.Context(), c.attemptOf(p.ID).externalID); err != nil {
		t.Fatal(err)
	}
	inbox := c.inbox()
	if len(inbox) != 1 || inbox[0].provider != "boletosim" || inbox[0].typ != "boleto.paid" {
		t.Fatalf("inbox = %+v, want one boleto.paid from boletosim", inbox)
	}
	c.processInbox()
	if got := c.inbox(); got[0].status != "processed" {
		t.Fatalf("event status = %s, want processed", got[0].status)
	}

	paid := boletoOf(t, c.do(getPayment(t, key, p.ID), true))
	if paid.Status != "paid" || paid.Boleto.Status != "ready" || paid.Boleto.Barcode != p.Boleto.Barcode {
		t.Errorf("after the event = %+v, want paid with the same boleto", paid)
	}
	c.checkLedger(0, 0)

	// A boleto has no way back of its own; the bank returns the money by transfer,
	// and the refund goes to the provider the boleto was paid through.
	r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+p.ID, "r1", usdBody(1000)), true))
	if r.Status != "succeeded" || r.PayerAmount.Value != p.Quote.PayerAmount.Value/2 {
		t.Errorf("refund = %+v, want succeeded with half of R$ %d", r, p.Quote.PayerAmount.Value)
	}
	if n := c.boletoProvider.refunds.Load(); n != 1 {
		t.Errorf("bank asked to refund %d times, want 1", n)
	}
}

// Decision 53: the bank refuses a boleto past its last day, and the payment expires
// with its quote once the expiry job has asked the bank.
func TestOverdueBoletoIsRefusedAndThePaymentExpires(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	p := boletoOf(t, c.do(postPayment(t, key, "order-125", boletoBody("order-125", maria)), true))

	c.clock.Advance(p.Quote.ExpiresAt.Sub(c.clock.Now())) // Thursday 5, 00:00 in São Paulo
	if _, err := c.boleto.Pay(t.Context(), c.attemptOf(p.ID).externalID); !errors.Is(err, boletosim.ErrOverdue) {
		t.Fatalf("paying after the due date: error = %v, want ErrOverdue", err)
	}
	expirer := paymentsapp.NewExpirer(paymentsadapters.NewPostgres(c.pool, paymentSinks()), c.deps.providers, c.clock,
		slog.New(slog.DiscardHandler), providerTimeout)
	if n, err := expirer.Expire(t.Context(), paymentsapp.ExpiryBatch); err != nil || n != 1 {
		t.Fatalf("Expire() = %d, %v; want 1", n, err)
	}
	got := boletoOf(t, c.do(getPayment(t, key, p.ID), true))
	if got.Status != "expired" || got.Boleto.Status != "ready" {
		t.Errorf("after expiry = %+v, want expired", got)
	}
	if n := c.count("ledger_entries"); n != 0 {
		t.Errorf("%d ledger entries for a boleto nobody paid", n)
	}
}

// Section 4.3: a lost answer leaves the boleto processing, and the resolver finds it
// at the bank by the attempt's ID without registering a second one.
func TestLostBoletoAnswerIsResolved(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	if err := c.boleto.InjectFault(t.Context(), boletosim.Fault{Op: boletosim.OpCreate, Kind: boletosim.TimeoutAfter, Count: 1}); err != nil {
		t.Fatal(err)
	}
	p := boletoOf(t, c.do(postPayment(t, key, "order-125", boletoBody("order-125", maria)), true))
	if p.Status != "pending" || p.Boleto.Status != "processing" || p.Boleto.DueDate != "2026-11-04" || c.attemptOf(p.ID).status != "unknown" {
		t.Fatalf("payment = %+v, boleto %+v; want pending and processing, due date known", p, *p.Boleto)
	}

	resolver := paymentsapp.NewResolver(paymentsadapters.NewPostgres(c.pool, paymentSinks()), c.deps.providers, c.clock,
		slog.New(slog.DiscardHandler), providerTimeout)
	c.clock.Advance(payments.CheckBase)
	if n, err := resolver.Resolve(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("Resolve() = %d, %v; want 1 claimed", n, err)
	}
	got := boletoOf(t, c.do(getPayment(t, key, p.ID), true))
	if got.Boleto.Status != "ready" || len(got.Boleto.Barcode) != boleto.BarcodeLength {
		t.Errorf("after the resolver = %+v, want ready", *got.Boleto)
	}
	if n := c.boletoProvider.creates.Load(); n != 1 {
		t.Errorf("bank asked to register %d times, want 1: unknown is never repeated", n)
	}
}

func TestBoletoPayerErrors(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
		valid      bool
	}{
		{"no payer", `{"amount": {"value": 2000, "currency": "USD"}, "method": "boleto", "reference": "o"}`, http.StatusUnprocessableEntity, "payer_required", true},
		{"wrong check digit", boletoBody("o", `{"name": "Maria", "document": "52998224726"}`), http.StatusUnprocessableEntity, "invalid_payer_document", true},
		{"blank name", boletoBody("o", `{"name": " ", "document": "52998224725"}`), http.StatusUnprocessableEntity, "invalid_payer_name", true},
		{"bad e-mail", boletoBody("o", `{"name": "Maria", "document": "52998224725", "email": "maria"}`), http.StatusUnprocessableEntity, "invalid_payer_email", true},
		{"pix with a payer", `{"amount": {"value": 2000, "currency": "USD"}, "method": "pix", "reference": "o", "payer": ` + maria + `}`, http.StatusUnprocessableEntity, "payer_not_allowed", true},
		{"boleto with a card", `{"amount": {"value": 2000, "currency": "USD"}, "method": "boleto", "reference": "o", "payer": ` + maria + `, "card": {"token": "tok_approved"}}`, http.StatusUnprocessableEntity, "card_not_allowed", true},
		{"empty payer", boletoBody("o", `{}`), http.StatusBadRequest, "invalid_request", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := c.do(postPayment(t, key, "k-"+tc.name, tc.body), tc.valid)
			if rec.Code != tc.status || problemCode(t, rec) != tc.code {
				t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
		})
	}
	if n := c.boletoProvider.creates.Load(); n != 0 {
		t.Errorf("bank called %d times for refused requests", n)
	}
}
