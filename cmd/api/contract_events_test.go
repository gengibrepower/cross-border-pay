package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

type inboxRow struct {
	provider, externalID, typ, status string
	payload                           []byte
}

func (c *contract) inbox() []inboxRow {
	c.t.Helper()
	rows, err := c.pool.Query(c.t.Context(), "SELECT provider, external_id, type, status, payload FROM inbox_events ORDER BY id")
	if err != nil {
		c.t.Fatal(err)
	}
	defer rows.Close()
	var got []inboxRow
	for rows.Next() {
		var r inboxRow
		if err := rows.Scan(&r.provider, &r.externalID, &r.typ, &r.status, &r.payload); err != nil {
			c.t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		c.t.Fatal(err)
	}
	return got
}

// The payer pays, the simulator delivers charge.paid over HTTP to the real route, and
// the event waits in the inbox for the worker; a redelivery adds nothing.
func TestPaidChargeReachesTheInbox(t *testing.T) {
	c := newContract(t)
	rec := c.do(postPayment(t, c.merchantKey(), "order-123", gameStore), true)
	attempt := c.attemptOf(paymentID(t, rec))
	if _, err := c.sim.Pay(t.Context(), attempt.externalID); err != nil {
		t.Fatal(err)
	}

	got := c.inbox()
	if len(got) != 1 {
		t.Fatalf("%d inbox events after paying, want 1", len(got))
	}
	var event pixsim.Event
	if err := json.Unmarshal(got[0].payload, &event); err != nil {
		t.Fatal(err)
	}
	if got[0].provider != "pixsim" || got[0].typ != "charge.paid" || got[0].status != "pending" ||
		got[0].externalID != event.ID || event.Charge.IdempotencyKey != attempt.id.String() {
		t.Errorf("inbox %+v with event %+v; want a pending charge.paid for %s", got[0], event, attempt.id)
	}

	if err := c.sim.Redeliver(t.Context(), event.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(c.inbox()); n != 1 {
		t.Errorf("%d inbox events after a redelivery, want 1", n)
	}
}

func postEvent(t *testing.T, provider, signature, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://localhost:8080/v1/providers/"+provider+"/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(pixsim.SignatureHeader, signature)
	return req
}

const signedBody = `{"id":"evt_1","type":"charge.paid","created_at":"2026-11-01T12:00:00Z","charge":{"id":"chg_1","idempotency_key":"att_1","status":"paid","amount":{"value":10150,"currency":"BRL"},"expires_at":"2026-11-01T12:30:00Z","created_at":"2026-11-01T12:00:00Z"}}`

func TestProviderEventResponsesMatchTheContract(t *testing.T) {
	c := newContract(t)
	sign := func(secret, body string) string { return pixsim.Sign([]byte(secret), c.clock.Now(), []byte(body)) }
	for _, tc := range []struct {
		name      string
		provider  string
		signature string
		body      string
		status    int
	}{
		{"signed", "pixsim", sign(webhookSecret, signedBody), signedBody, http.StatusOK},
		{"redelivered", "pixsim", sign(webhookSecret, signedBody), signedBody, http.StatusOK},
		{"forged", "pixsim", sign("guess", signedBody), signedBody, http.StatusUnauthorized},
		{"unsigned", "pixsim", "", signedBody, http.StatusUnauthorized},
		{"signed but unreadable", "pixsim", sign(webhookSecret, `{"id":1}`), `{"id":1}`, http.StatusBadRequest},
		{"unknown provider", "acme", sign(webhookSecret, signedBody), signedBody, http.StatusNotFound},
	} {
		if rec := c.do(postEvent(t, tc.provider, tc.signature, tc.body), true); rec.Code != tc.status {
			t.Errorf("%s: got %d %s, want %d", tc.name, rec.Code, rec.Body, tc.status)
		}
	}
	if n := len(c.inbox()); n != 1 {
		t.Errorf("%d inbox events, want only the signed one", n)
	}
}
