package main

import (
	"encoding/json"
	"net/http"
	"testing"

	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// The payment.paid in the outbox is what api/openapi.yaml's PaymentEvent describes, and
// it agrees with the GET of the payment it reports.
func TestPaymentPaidMatchesTheContract(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id, _ := c.createPayment(key)

	var attempt payments.AttemptID
	var charge, brCode string
	err := c.pool.QueryRow(t.Context(), `SELECT a.id, a.external_id, a.br_code FROM payment_attempts AS a
		JOIN payments AS p ON p.id = a.payment_id WHERE p.id = (SELECT id FROM payments)`).Scan(&attempt, &charge, &brCode)
	if err != nil {
		t.Fatal(err)
	}
	store := paymentsadapters.NewPostgres(c.pool, merchantOutbox{})
	u := payments.AttemptUpdate{Event: payments.Succeed, ExternalID: charge, BRCode: brCode}
	if _, _, err := store.ApplyAttempt(t.Context(), attempt, u, c.clock.Now()); err != nil {
		t.Fatal(err)
	}

	var payload []byte
	var subject string
	if err := c.pool.QueryRow(t.Context(), "SELECT payload, subject_id FROM outbox_events").Scan(&payload, &subject); err != nil {
		t.Fatal(err)
	}
	var event any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if err := c.doc.Components.Schemas["PaymentEvent"].Value.VisitJSON(event); err != nil {
		t.Errorf("payload does not match PaymentEvent: %v\n%s", err, payload)
	}

	rec := c.do(getRequest(t, key, "/v1/payments/"+id), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	var got struct {
		ID     string          `json:"id"`
		Status string          `json:"status"`
		Amount json.RawMessage `json:"amount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var e struct {
		PaymentID      string `json:"payment_id"`
		PaymentVersion int    `json:"payment_version"`
		Data           struct {
			Status string          `json:"status"`
			Amount json.RawMessage `json:"amount"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &e); err != nil {
		t.Fatal(err)
	}
	if e.PaymentID != got.ID || subject != got.ID || e.Data.Status != got.Status || got.Status != "paid" ||
		string(e.Data.Amount) != string(got.Amount) || e.PaymentVersion != 2 {
		t.Errorf("event %s (subject %s) disagrees with GET %s", payload, subject, rec.Body)
	}
}
