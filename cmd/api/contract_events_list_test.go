package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	merchantsadapters "github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	merchants "github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// paid creates a payment for the merchant and confirms it, which puts a payment.paid
// in the outbox, and returns that event's public ID.
func (c *contract) paid(key string) string {
	c.t.Helper()
	publicID, _ := c.createPayment(key)
	id, err := payments.ParsePaymentID(publicID)
	if err != nil {
		c.t.Fatal(err)
	}
	var attempt payments.AttemptID
	var charge, brCode string
	var amount int64
	err = c.pool.QueryRow(c.t.Context(), `SELECT a.id, a.external_id, a.br_code, q.payer_amount
		FROM payment_attempts a JOIN payments p ON p.id = a.payment_id JOIN quotes q ON q.id = p.quote_id
		WHERE p.id = $1`, id).Scan(&attempt, &charge, &brCode, &amount)
	if err != nil {
		c.t.Fatal(err)
	}
	u := payments.AttemptUpdate{Event: payments.Succeed, ExternalID: charge, BRCode: brCode, PaidAt: c.clock.Now(), Amount: money.New(amount, money.BRL)}
	if _, _, err := paymentsadapters.NewPostgres(c.pool, paymentSinks()).ApplyAttempt(c.t.Context(), attempt, &u, c.clock.Now()); err != nil {
		c.t.Fatal(err)
	}
	var payload []byte
	if err := c.pool.QueryRow(c.t.Context(), "SELECT payload FROM outbox_events WHERE subject_id = $1", publicID).Scan(&payload); err != nil {
		c.t.Fatal(err)
	}
	var e struct{ ID string }
	if err := json.Unmarshal(payload, &e); err != nil {
		c.t.Fatal(err)
	}
	return e.ID
}

type outboxEvent struct {
	Event    json.RawMessage `json:"event"`
	Delivery struct {
		Status         string     `json:"status"`
		Attempts       int        `json:"attempts"`
		NextAttemptAt  *time.Time `json:"next_attempt_at"`
		LastStatusCode *int       `json:"last_status_code"`
		LastError      *string    `json:"last_error"`
		FinishedAt     *time.Time `json:"finished_at"`
	} `json:"delivery"`
}

func (e *outboxEvent) id() string {
	var body struct{ ID string }
	_ = json.Unmarshal(e.Event, &body)
	return body.ID
}

type eventList struct {
	Data       []outboxEvent `json:"data"`
	NextCursor *string       `json:"next_cursor"`
}

func (c *contract) events(key, query string, status int) eventList {
	c.t.Helper()
	rec := c.do(getRequest(c.t, key, "/v1/events"+query), status == http.StatusOK)
	if rec.Code != status {
		c.t.Fatalf("GET /v1/events%s: status %d, want %d: %s", query, rec.Code, status, rec.Body)
	}
	var l eventList
	if status == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
			c.t.Fatal(err)
		}
	}
	return l
}

func (c *contract) redeliver(key, id string, status int) (e outboxEvent, code string) {
	c.t.Helper()
	req := httptest.NewRequestWithContext(c.t.Context(), http.MethodPost, "http://localhost:8080/v1/events/"+id+"/redeliver", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := c.do(req, true)
	if rec.Code != status {
		c.t.Fatalf("POST redeliver %s: status %d, want %d: %s", id, rec.Code, status, rec.Body)
	}
	var p struct{ Code string }
	if status == http.StatusAccepted {
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			c.t.Fatal(err)
		}
	} else if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		c.t.Fatal(err)
	}
	return e, p.Code
}

// Seven events in pages of 3: each seen once, newest first, each event the stored
// bytes, and none of another merchant's.
func TestEventsListPagesTheMerchantsOwnEvents(t *testing.T) {
	c := newContract(t)
	key, other := c.merchantKey(), c.merchantKey()
	want := map[string]bool{}
	for range 7 {
		want[c.paid(key)] = true
	}
	foreign := c.paid(other)

	var seen []string
	cursor := ""
	for page := 0; ; page++ {
		l := c.events(key, "?limit=3"+cursor, http.StatusOK)
		if page < 2 && (len(l.Data) != 3 || l.NextCursor == nil) || page == 2 && (len(l.Data) != 1 || l.NextCursor != nil) {
			t.Fatalf("page %d: %d events, next_cursor %v", page, len(l.Data), l.NextCursor)
		}
		for i := range l.Data {
			e := &l.Data[i]
			var stored []byte
			if err := c.pool.QueryRow(t.Context(), "SELECT payload FROM outbox_events WHERE payload = $1", []byte(e.Event)).Scan(&stored); err != nil {
				t.Errorf("event %s is not the stored bytes: %v", e.Event, err)
			}
			if e.Delivery.Status != "pending" || e.Delivery.Attempts != 0 || e.Delivery.NextAttemptAt == nil {
				t.Errorf("delivery = %+v, want pending and never tried", e.Delivery)
			}
			seen = append(seen, e.id())
		}
		if l.NextCursor == nil {
			break
		}
		cursor = "&cursor=" + *l.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("saw %d events, want 7", len(seen))
	}
	for i, id := range seen {
		if !want[id] || id == foreign {
			t.Errorf("event %s is not one of the merchant's", id)
		}
		if i > 0 && seen[i-1] <= id {
			t.Errorf("%s listed before %s: not newest first", seen[i-1], id)
		}
		delete(want, id)
	}
}

func TestEventsListFiltersByDeliveryStatus(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	c.paid(key)
	failed := c.paid(key)
	_, err := c.pool.Exec(t.Context(), `UPDATE outbox_events SET delivery_status = 'failed', next_attempt_at = NULL,
		finished_at = $2, attempts = 22, last_status_code = 500, last_error = 'answered 500' WHERE convert_from(payload, 'UTF8')::jsonb->>'id' = $1`,
		failed, c.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	l := c.events(key, "?delivery_status=failed", http.StatusOK)
	if len(l.Data) != 1 || l.Data[0].id() != failed {
		t.Fatalf("failed events = %+v, want only %s", l.Data, failed)
	}
	d := l.Data[0].Delivery
	if d.Attempts != 22 || *d.LastStatusCode != 500 || *d.LastError != "answered 500" || d.NextAttemptAt != nil || !d.FinishedAt.Equal(c.clock.Now()) {
		t.Errorf("delivery = %+v", d)
	}
	if n := len(c.events(key, "?delivery_status=pending", http.StatusOK).Data); n != 1 {
		t.Errorf("%d pending, want 1", n)
	}
	if n := len(c.events(key, "?delivery_status=delivered", http.StatusOK).Data); n != 0 {
		t.Errorf("%d delivered, want 0", n)
	}
}

func TestEventsListRefusesBadParameters(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	for query, code := range map[string]string{
		"?limit=0": "invalid_limit", "?limit=101": "invalid_limit", "?limit=ten": "invalid_limit",
		"?delivery_status=sent": "invalid_delivery_status", "?cursor=@@": "invalid_cursor", "?cursor=AAAA": "invalid_cursor",
	} {
		rec := c.do(getRequest(t, key, "/v1/events"+query), false)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
			t.Errorf("%s: %d %s, want 400 %s", query, rec.Code, rec.Body, code)
		}
	}
	if rec := c.do(getRequest(t, "", "/v1/events"), false); rec.Code != http.StatusUnauthorized {
		t.Errorf("without a key: %d, want 401", rec.Code)
	}
}

// Redelivery from each status: queued due now, with a 409 when there is nowhere to
// deliver and the same 404 for another merchant's event, an unknown one and a
// malformed ID.
func TestRedeliverQueuesTheEventAgain(t *testing.T) {
	c := newContract(t)
	key, other := c.merchantKey(), c.merchantKey()
	id := c.paid(key)

	if _, code := c.redeliver(key, id, http.StatusConflict); code != "webhook_endpoint_not_found" {
		t.Errorf("without an endpoint: %s", code)
	}
	c.putEndpoint(key, `{"url":"https://gamestore.example/hooks"}`, http.StatusOK)

	for _, status := range []string{"failed", "delivered", "pending"} {
		_, err := c.pool.Exec(t.Context(), `UPDATE outbox_events SET delivery_status = $2,
			next_attempt_at = CASE WHEN $2 = 'pending' THEN $3::timestamptz + interval '6 hours' END,
			finished_at = CASE WHEN $2 = 'pending' THEN NULL ELSE $3::timestamptz END, attempts = 5
			WHERE convert_from(payload, 'UTF8')::jsonb->>'id' = $1`, id, status, c.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		c.clock.Advance(time.Minute)
		e, _ := c.redeliver(key, id, http.StatusAccepted)
		if e.id() != id || e.Delivery.Status != "pending" || e.Delivery.FinishedAt != nil || !e.Delivery.NextAttemptAt.Equal(c.clock.Now()) {
			t.Errorf("from %s: %s %+v, want pending, due now", status, e.id(), e.Delivery)
		}
		var started time.Time
		var first int
		err = c.pool.QueryRow(t.Context(), "SELECT window_started_at, window_first_attempt FROM outbox_events WHERE convert_from(payload, 'UTF8')::jsonb->>'id' = $1", id).
			Scan(&started, &first)
		if err != nil || !started.Equal(c.clock.Now()) || first != 6 {
			t.Errorf("from %s: window from %v at attempt %d (%v), want now at 6", status, started, first, err)
		}
	}

	for _, unknown := range []string{c.paid(other), "evt_00000000000000000000000000", "evt_nope", "pay_" + id[4:]} {
		if _, code := c.redeliver(key, unknown, http.StatusNotFound); code != "event_not_found" {
			t.Errorf("%s: %s, want event_not_found", unknown, code)
		}
	}

	merchant := c.merchantOf("https://gamestore.example/hooks")
	store := merchantsadapters.NewPostgres(c.pool)
	for _, at := range []time.Duration{0, merchants.DisableAfter} {
		if _, err := store.EndpointFailed(t.Context(), merchant, "answered 500", c.clock.Now().Add(at)); err != nil {
			t.Fatal(err)
		}
	}
	if _, code := c.redeliver(key, id, http.StatusConflict); code != "webhook_endpoint_disabled" {
		t.Errorf("with the endpoint disabled: %s", code)
	}
}
