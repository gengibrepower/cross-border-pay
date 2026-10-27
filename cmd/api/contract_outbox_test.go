package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// checkEvent finds the outbox event of type t, checks it against api/openapi.yaml's
// PaymentEvent and against the GET of the payment it reports.
func (c *contract) checkEvent(t, key, id, status string) {
	c.t.Helper()
	var payload []byte
	var subject string
	err := c.pool.QueryRow(c.t.Context(), "SELECT payload, subject_id FROM outbox_events WHERE type = $1", t).Scan(&payload, &subject)
	if err != nil {
		c.t.Fatalf("no %s in the outbox: %v", t, err)
	}
	var event any
	if err := json.Unmarshal(payload, &event); err != nil {
		c.t.Fatal(err)
	}
	if err := c.doc.Components.Schemas["PaymentEvent"].Value.VisitJSON(event); err != nil {
		c.t.Errorf("%s does not match PaymentEvent: %v\n%s", t, err, payload)
	}

	rec := c.do(getRequest(c.t, key, "/v1/payments/"+id), true)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	var got struct {
		ID     string          `json:"id"`
		Status string          `json:"status"`
		Amount json.RawMessage `json:"amount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		c.t.Fatal(err)
	}
	var e struct {
		Type           string `json:"type"`
		PaymentID      string `json:"payment_id"`
		PaymentVersion int    `json:"payment_version"`
		Data           struct {
			Status string          `json:"status"`
			Amount json.RawMessage `json:"amount"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &e); err != nil {
		c.t.Fatal(err)
	}
	if e.Type != t || e.PaymentID != got.ID || subject != got.ID || e.Data.Status != got.Status || got.Status != status ||
		string(e.Data.Amount) != string(got.Amount) || e.PaymentVersion != 2 {
		c.t.Errorf("event %s (subject %s) disagrees with GET %s, want %s at version 2", payload, subject, rec.Body, status)
	}
}

func TestPaymentPaidMatchesTheContract(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id, _ := c.createPayment(key)

	var attempt payments.AttemptID
	var charge, brCode string
	var amount int64
	err := c.pool.QueryRow(t.Context(), `SELECT a.id, a.external_id, a.br_code, q.payer_amount
		FROM payment_attempts a JOIN payments p ON p.id = a.payment_id JOIN quotes q ON q.id = p.quote_id`).Scan(&attempt, &charge, &brCode, &amount)
	if err != nil {
		t.Fatal(err)
	}
	store := paymentsadapters.NewPostgres(c.pool, paymentSinks())
	u := payments.AttemptUpdate{Event: payments.Succeed, ExternalID: charge, BRCode: brCode, PaidAt: c.clock.Now(), Amount: money.New(amount, money.BRL)}
	if _, _, err := store.ApplyAttempt(t.Context(), attempt, &u, c.clock.Now()); err != nil {
		t.Fatal(err)
	}
	c.checkEvent("payment.paid", key, id, "paid")
}

// The worker's expiry job, against the API's own simulator and database.
func TestPaymentExpiredMatchesTheContract(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id, _ := c.createPayment(key)

	expirer := paymentsapp.NewExpirer(paymentsadapters.NewPostgres(c.pool, paymentSinks()), c.provider, c.clock,
		slog.New(slog.DiscardHandler), providerTimeout)
	c.clock.Advance(payments.PixQuoteValidity - time.Microsecond)
	if n, err := expirer.Expire(t.Context(), paymentsapp.ExpiryBatch); err != nil || n != 0 {
		t.Fatalf("Expire() a microsecond early = %d, %v; want nothing claimed", n, err)
	}
	c.clock.Advance(time.Microsecond)
	if n, err := expirer.Expire(t.Context(), paymentsapp.ExpiryBatch); err != nil || n != 1 {
		t.Fatalf("Expire() = %d, %v; want 1 payment", n, err)
	}
	c.checkEvent("payment.expired", key, id, "expired")
}
