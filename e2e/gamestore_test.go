package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/webhooks/domain"
)

// merchantServer plays every merchant's webhook endpoint, one path each. It checks each
// signature with that merchant's secret and keeps what it accepted.
type merchantServer struct {
	*httptest.Server
	mu       sync.Mutex
	secrets  map[string]string
	down     map[string]*atomic.Bool
	received map[string][][]byte // by path
	forged   atomic.Int64
}

func newMerchantServer(t *testing.T) *merchantServer {
	t.Helper()
	m := &merchantServer{secrets: map[string]string{}, down: map[string]*atomic.Bool{}, received: map[string][][]byte{}}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.Close)
	return m
}

func (m *merchantServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	secret, down := m.secrets[r.URL.Path], m.down[r.URL.Path]
	m.mu.Unlock()
	if down != nil && down.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if domain.Verify(secret, r.Header.Get(domain.SignatureHeader), body, time.Now()) != nil {
		m.forged.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	m.mu.Lock()
	m.received[r.URL.Path] = append(m.received[r.URL.Path], body)
	m.mu.Unlock()
}

func (m *merchantServer) bodies(path string) [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.received[path]...)
}

// register gives the merchant behind key an endpoint at path with PUT
// /v1/webhook-endpoint, and keeps the secret the answer shows once.
func (s *stack) register(key, path string) *atomic.Bool {
	s.t.Helper()
	status, _, body := s.do(http.MethodPut, s.api+"/v1/webhook-endpoint", key, map[string]string{"url": s.merchant.URL + path})
	var e struct{ Secret string }
	if status != http.StatusOK || json.Unmarshal(body, &e) != nil || e.Secret == "" {
		s.t.Fatalf("PUT /v1/webhook-endpoint: %d %s", status, body)
	}
	down := &atomic.Bool{}
	s.merchant.mu.Lock()
	s.merchant.secrets[path], s.merchant.down[path] = e.Secret, down
	s.merchant.mu.Unlock()
	return down
}

type payment struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Amount struct {
		Value    int64  `json:"value"`
		Currency string `json:"currency"`
	} `json:"amount"`
	Quote struct {
		PayerAmount struct {
			Value int64 `json:"value"`
		} `json:"payer_amount"`
	} `json:"quote"`
	Pix struct {
		Status string `json:"status"`
		BRCode string `json:"br_code"`
	} `json:"pix"`
}

func (s *stack) createPayment(key, order string) (p payment, body []byte) {
	s.t.Helper()
	status, _, body := s.do(http.MethodPost, s.api+"/v1/payments", key,
		map[string]any{"amount": map[string]any{"value": 2000, "currency": "USD"}, "method": "pix", "reference": order},
		"Idempotency-Key", order)
	if status != http.StatusCreated || json.Unmarshal(body, &p) != nil {
		s.t.Fatalf("POST /v1/payments: %d %s", status, body)
	}
	return p, body
}

func (s *stack) getPayment(key, id string) (p payment, body []byte) {
	s.t.Helper()
	status, _, body := s.do(http.MethodGet, s.api+"/v1/payments/"+id, key, nil)
	if status != http.StatusOK || json.Unmarshal(body, &p) != nil {
		s.t.Fatalf("GET /v1/payments/%s: %d %s", id, status, body)
	}
	return p, body
}

// pay is the payer: it pays the charge behind the BR Code it was shown.
func (s *stack) pay(brCode string) {
	s.t.Helper()
	charge := s.charges.charge(brCode)
	if charge == "" {
		s.t.Fatalf("no charge seen for BR Code %s", brCode)
	}
	if status, _, body := s.do(http.MethodPost, s.pixsim+"/control/charges/"+charge+"/pay", "", nil); status != http.StatusOK {
		s.t.Fatalf("pay %s: %d %s", charge, status, body)
	}
}

func (s *stack) waitPaid(key, id string) {
	s.t.Helper()
	eventually(s.t, 15*time.Second, id+" paid", func() bool {
		p, _ := s.getPayment(key, id)
		return p.Status == "paid"
	})
}

type webhook struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	PaymentID      string `json:"payment_id"`
	PaymentVersion int    `json:"payment_version"`
	Data           struct {
		Status string          `json:"status"`
		Amount json.RawMessage `json:"amount"`
	} `json:"data"`
}

// ledger checks invariant 1 over the whole database, the books summing to zero in
// each currency, and returns how many confirmations were posted. There is no balance
// API before F7, so this one look goes to SQL.
func (s *stack) ledger() int {
	s.t.Helper()
	rows, err := s.pool.Query(s.t.Context(), "SELECT currency, sum(amount) FROM ledger_entries GROUP BY currency")
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var currency string
		var sum int64
		if err := rows.Scan(&currency, &sum); err != nil {
			s.t.Fatal(err)
		}
		if sum != 0 {
			s.t.Errorf("%s entries sum to %d, want 0", currency, sum)
		}
	}
	var confirmations int
	if err := s.pool.QueryRow(s.t.Context(), "SELECT count(*) FROM ledger_transactions WHERE reference LIKE 'payment:%:confirm'").Scan(&confirmations); err != nil {
		s.t.Fatal(err)
	}
	return confirmations
}

// Section 4.1, steps 1 to 7: create with an idempotency key, repeat, pay, confirm,
// post to the ledger and notify the merchant with a signed webhook. A duplicate event
// from the provider changes nothing.
func TestGameStoreWalkthrough(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	key := s.createMerchant("GameStore")
	s.register(key, "/gamestore")

	created, first := s.createPayment(key, "order-123")
	if created.Status != "pending" || created.Pix.Status != "ready" || created.Pix.BRCode == "" || created.Quote.PayerAmount.Value != 10150 {
		t.Fatalf("created %s, want pending with a BR Code for R$ 101,50", first)
	}
	_, header, again := s.do(http.MethodPost, s.api+"/v1/payments", key,
		map[string]any{"amount": map[string]any{"value": 2000, "currency": "USD"}, "method": "pix", "reference": "order-123"},
		"Idempotency-Key", "order-123")
	if !bytes.Equal(first, again) || header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("the repeat answered %s, want the stored answer replayed", again)
	}

	s.pay(created.Pix.BRCode)
	s.waitPaid(key, created.ID)
	eventually(t, 15*time.Second, "payment.paid at the merchant", func() bool { return len(s.merchant.bodies("/gamestore")) == 1 })
	var w webhook
	if err := json.Unmarshal(s.merchant.bodies("/gamestore")[0], &w); err != nil {
		t.Fatal(err)
	}
	paid, _ := s.getPayment(key, created.ID)
	amount, _ := json.Marshal(paid.Amount)
	if w.Type != "payment.paid" || w.PaymentID != created.ID || w.PaymentVersion != 2 || w.Data.Status != "paid" || !bytes.Equal(w.Data.Amount, amount) {
		t.Errorf("webhook %+v disagrees with the payment %+v", w, paid)
	}
	if n := s.ledger(); n != 1 {
		t.Errorf("%d confirmations in the ledger, want 1", n)
	}

	// The provider sends the same event again: the inbox drops it.
	sent := s.inbox.sent()
	if len(sent) != 1 {
		t.Fatalf("the simulator sent %d events, want 1", len(sent))
	}
	if status, _, body := s.do(http.MethodPost, s.pixsim+"/control/events/"+sent[0]+"/deliver", "", nil); status != http.StatusNoContent {
		t.Fatalf("redeliver %s: %d %s", sent[0], status, body)
	}
	time.Sleep(3 * time.Second) // a few worker rounds, for a duplicate effect to show
	if n := s.ledger(); n != 1 {
		t.Errorf("%d confirmations after the duplicate event, want 1", n)
	}
	if n := len(s.merchant.bodies("/gamestore")); n != 1 {
		t.Errorf("the merchant got %d webhooks after the duplicate event, want 1", n)
	}
	status, _, list := s.do(http.MethodGet, s.api+"/v1/events", key, nil)
	var events struct {
		Data []struct {
			Delivery struct{ Status string } `json:"delivery"`
		} `json:"data"`
	}
	if status != http.StatusOK || json.Unmarshal(list, &events) != nil || len(events.Data) != 1 || events.Data[0].Delivery.Status != "delivered" {
		t.Errorf("GET /v1/events: %d %s, want the one event, delivered", status, list)
	}
	if s.merchant.forged.Load() != 0 {
		t.Errorf("%d webhooks with a bad signature", s.merchant.forged.Load())
	}
	s.stop()
}

// Decision 17 across processes: the simulator takes the charge and never answers, the
// API answers with the attempt still processing, and the worker's resolver finds the
// charge by the attempt's key, so the payer can pay it.
func TestAnAttemptLostToATimeoutIsResolvedByTheWorker(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	key := s.createMerchant("GameStore")
	s.register(key, "/gamestore")
	if status, _, body := s.do(http.MethodPost, s.pixsim+"/control/faults", "", map[string]any{"op": "create", "fault": "timeout_after", "count": 1}); status != http.StatusNoContent {
		t.Fatalf("inject fault: %d %s", status, body)
	}

	created, body := s.createPayment(key, "order-lost")
	if created.Status != "pending" || created.Pix.Status != "processing" || created.Pix.BRCode != "" {
		t.Fatalf("created %s, want the attempt still processing", body)
	}
	var ready payment
	eventually(t, 20*time.Second, "the attempt resolved", func() bool {
		ready, _ = s.getPayment(key, created.ID)
		return ready.Pix.Status == "ready"
	})
	s.pay(ready.Pix.BRCode)
	s.waitPaid(key, created.ID)
	eventually(t, 15*time.Second, "payment.paid at the merchant", func() bool { return len(s.merchant.bodies("/gamestore")) == 1 })
	if n := s.ledger(); n != 1 {
		t.Errorf("%d confirmations in the ledger, want 1", n)
	}
	s.stop()
}

// A merchant that is down does not hold the others back: GameStore gets its webhook
// while Arcade's waits in its backoff, and once Arcade is back, a redelivery by hand
// sends it at once instead of after the backoff.
func TestADownMerchantDoesNotDelayTheOthers(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	gamestore, arcade := s.createMerchant("GameStore"), s.createMerchant("Arcade")
	s.register(gamestore, "/gamestore")
	arcadeDown := s.register(arcade, "/arcade")
	arcadeDown.Store(true)

	a, _ := s.createPayment(arcade, "arcade-1")
	g, _ := s.createPayment(gamestore, "gamestore-1")
	s.pay(a.Pix.BRCode)
	s.pay(g.Pix.BRCode)
	s.waitPaid(arcade, a.ID)
	s.waitPaid(gamestore, g.ID)

	eventually(t, 15*time.Second, "GameStore's webhook", func() bool { return len(s.merchant.bodies("/gamestore")) == 1 })
	type listed struct {
		Data []struct {
			Event    struct{ ID string } `json:"event"`
			Delivery struct {
				Status         string `json:"status"`
				Attempts       int    `json:"attempts"`
				LastStatusCode *int   `json:"last_status_code"`
			} `json:"delivery"`
		} `json:"data"`
	}
	var pending listed
	eventually(t, 15*time.Second, "Arcade's failed attempt recorded", func() bool {
		_, _, body := s.do(http.MethodGet, s.api+"/v1/events?delivery_status=pending", arcade, nil)
		pending = listed{}
		return json.Unmarshal(body, &pending) == nil && len(pending.Data) == 1 &&
			pending.Data[0].Delivery.LastStatusCode != nil && *pending.Data[0].Delivery.LastStatusCode == http.StatusServiceUnavailable
	})
	if n := len(s.merchant.bodies("/arcade")); n != 0 {
		t.Fatalf("Arcade, down, accepted %d webhooks", n)
	}

	arcadeDown.Store(false)
	redelivered := time.Now()
	id := pending.Data[0].Event.ID
	if status, _, body := s.do(http.MethodPost, fmt.Sprintf("%s/v1/events/%s/redeliver", s.api, id), arcade, nil); status != http.StatusAccepted {
		t.Fatalf("redeliver %s: %d %s", id, status, body)
	}
	eventually(t, 10*time.Second, "Arcade's webhook after the redelivery", func() bool { return len(s.merchant.bodies("/arcade")) == 1 })
	if d := time.Since(redelivered); d > 5*time.Second {
		t.Errorf("Arcade got its webhook %v after the redelivery, want within a few worker rounds", d)
	}
	if n := s.ledger(); n != 2 {
		t.Errorf("%d confirmations in the ledger, want 2", n)
	}
	s.stop()
}
