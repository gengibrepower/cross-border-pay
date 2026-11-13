package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	idempotency "github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
)

func cardBody(token string) string {
	return fmt.Sprintf(`{"amount": {"value": 2000, "currency": "USD"}, "method": "card", "reference": "order-123", "card": {"token": %q}}`, token)
}

type cardPayment struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Method string `json:"method"`
	Card   *struct {
		Status        string `json:"status"`
		CaptureMethod string `json:"capture_method"`
		Captured      *struct {
			Amount      struct{ Value int64 } `json:"amount"`
			PayerAmount struct{ Value int64 } `json:"payer_amount"`
			Fee         struct{ Value int64 } `json:"fee"`
			NetAmount   struct{ Value int64 } `json:"net_amount"`
		} `json:"captured"`
	} `json:"card"`
	Pix   *json.RawMessage `json:"pix"`
	Quote struct {
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"quote"`
}

func cardOf(t *testing.T, rec *httptest.ResponseRecorder) cardPayment {
	t.Helper()
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var p cardPayment
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Card == nil || p.Pix != nil {
		t.Fatalf("a card payment must carry card and no pix: %s", rec.Body)
	}
	return p
}

func getPayment(t *testing.T, key, id string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:8080/v1/payments/"+id, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

// Decision 18: a card is authorized and captured as the payment is created, so the
// answer is already paid, with the GameStore's books posted as for a PIX payment.
func TestCardPaymentIsPaidOnCreate(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	rec := c.do(postPayment(t, key, "order-123", cardBody(cardsim.TokenApproved)), true)
	p := cardOf(t, rec)
	if p.Status != "paid" || p.Method != "card" || p.Card.Status != "captured" {
		t.Fatalf("card payment = %+v, want paid and captured", p)
	}
	if !p.Quote.ExpiresAt.Equal(t0.Add(payments.CardQuoteValidity)) {
		t.Errorf("quote expires %v, want %v", p.Quote.ExpiresAt, t0.Add(payments.CardQuoteValidity))
	}
	if got := c.do(getPayment(t, key, p.ID), true); !bytes.Equal(got.Body.Bytes(), rec.Body.Bytes()) {
		t.Errorf("GET differs from the POST:\n%s\n%s", got.Body, rec.Body)
	}
	if again := c.do(postPayment(t, key, "order-123", cardBody(cardsim.TokenApproved)), true); !bytes.Equal(again.Body.Bytes(), rec.Body.Bytes()) {
		t.Errorf("retry differs from the first answer:\n%s\n%s", again.Body, rec.Body)
	}
	if n := c.cardProvider.creates.Load(); n != 1 || c.provider.creates.Load() != 0 {
		t.Errorf("acquirer called %d times and PIX %d, want 1 and 0", n, c.provider.creates.Load())
	}
	a := c.attemptOf(p.ID)
	if a.status != "succeeded" || a.externalID == "" {
		t.Errorf("attempt = %+v, want succeeded with the authorization's ID", a)
	}
	var provider, token string
	if err := c.pool.QueryRow(t.Context(), "SELECT provider, card_token FROM payment_attempts").Scan(&provider, &token); err != nil {
		t.Fatal(err)
	}
	if provider != "cardsim" || token != cardsim.TokenApproved {
		t.Errorf("attempt at %s with token %s, want cardsim with %s", provider, token, cardsim.TokenApproved)
	}
	c.checkLedger(0, 0)
	if n := c.count("outbox_events WHERE type = 'payment.paid'"); n != 1 {
		t.Errorf("%d payment.paid events, want 1", n)
	}
}

// A decline is the issuer's answer: the payment exists, pending, and nothing moved.
func TestDeclinedCardLeavesThePaymentPending(t *testing.T) {
	for _, token := range []string{cardsim.TokenDeclined, cardsim.TokenInsufficientFunds, "tok_unknown"} {
		t.Run(token, func(t *testing.T) {
			c := newContract(t)
			p := cardOf(t, c.do(postPayment(t, c.merchantKey(), "order-123", cardBody(token)), true))
			if p.Status != "pending" || p.Card.Status != "declined" {
				t.Errorf("payment = %+v, want pending and declined", p)
			}
			if n := c.count("ledger_entries"); n != 0 {
				t.Errorf("%d ledger entries after a decline", n)
			}
		})
	}
}

// Only a token gets in, only on a card payment; no rejected request stores anything.
func TestCardRequestErrors(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	for _, tc := range []struct {
		name         string
		body         string
		validRequest bool
		status       int
		code         string
	}{
		{"card without a card", `{"amount": {"value": 2000, "currency": "USD"}, "method": "card", "reference": "o"}`, true, http.StatusUnprocessableEntity, "card_required"},
		{"card number", cardBody("4111111111111111"), true, http.StatusUnprocessableEntity, "invalid_card_token"},
		{"empty token", cardBody(""), false, http.StatusBadRequest, "invalid_request"},
		{"pix with a card", `{"amount": {"value": 2000, "currency": "USD"}, "method": "pix", "reference": "o", "card": {"token": "tok_approved"}}`, true, http.StatusUnprocessableEntity, "card_not_allowed"},
		{"card with a number field", `{"amount": {"value": 2000, "currency": "USD"}, "method": "card", "reference": "o", "card": {"token": "tok_approved", "number": "4111111111111111"}}`, false, http.StatusBadRequest, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.t = t
			rec := c.do(postPayment(t, key, tc.name, tc.body), tc.validRequest)
			var p struct{ Code string }
			_ = json.Unmarshal(rec.Body.Bytes(), &p)
			if rec.Code != tc.status || p.Code != tc.code {
				t.Errorf("got %d %q, want %d %q: %s", rec.Code, p.Code, tc.status, tc.code, rec.Body)
			}
		})
	}
	c.t = t
	if n := c.count("payments"); n != 0 {
		t.Errorf("%d payments stored by rejected requests", n)
	}
	if n := c.cardProvider.creates.Load(); n != 0 {
		t.Errorf("acquirer called %d times by rejected requests", n)
	}
}

// The authorization went through and the answer was lost: the payment waits in
// processing, and the resolver finds the capture by the attempt's key and confirms it.
func TestAmbiguousCardIsConfirmedByTheResolver(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	if err := c.card.InjectFault(t.Context(), cardsim.Fault{Op: cardsim.OpAuthorize, Kind: cardsim.TimeoutAfter, Count: 1}); err != nil {
		t.Fatal(err)
	}
	p := cardOf(t, c.do(postPayment(t, key, "order-123", cardBody(cardsim.TokenApproved)), true))
	if p.Status != "pending" || p.Card.Status != "processing" || c.attemptOf(p.ID).status != "unknown" {
		t.Fatalf("payment = %+v, attempt %s; want pending, processing and unknown", p, c.attemptOf(p.ID).status)
	}

	resolver := paymentsapp.NewResolver(paymentsadapters.NewPostgres(c.pool, paymentSinks()), c.deps.providers, c.clock,
		slog.New(slog.DiscardHandler), providerTimeout)
	c.clock.Advance(payments.CheckBase)
	if n, err := resolver.Resolve(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("Resolve() = %d, %v; want 1 claimed", n, err)
	}
	got := cardOf(t, c.do(getPayment(t, key, p.ID), true))
	if got.Status != "paid" || got.Card.Status != "captured" {
		t.Errorf("after the resolver = %+v, want paid and captured", got)
	}
	if n := c.cardProvider.creates.Load(); n != 1 {
		t.Errorf("acquirer asked to authorize %d times, want 1: unknown is never repeated", n)
	}
	c.checkLedger(0, 0)
}

// A request that died after its first transaction left the attempt created with its
// token; the retry authorizes with that token, once.
func TestResumedCardCreateUsesTheStoredToken(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	body := cardBody(cardsim.TokenApproved)
	_, crashed := c.crashedCreateOf(key, "order-123", body, "card", payments.CardInput{Token: cardsim.TokenApproved}, paymentsadapters.CardSimName)

	c.clock.Advance(idempotency.Lease)
	p := cardOf(t, c.do(postPayment(t, key, "order-123", body), true))
	if p.ID != crashed.Payment.ID().String() || p.Status != "paid" || p.Card.Status != "captured" {
		t.Errorf("resumed create = %+v, want %s paid", p, crashed.Payment.ID())
	}
	if n := c.cardProvider.creates.Load(); n != 1 {
		t.Errorf("acquirer called %d times, want 1", n)
	}
}

// A card payment is refunded by the acquirer that captured it, with the same books as
// a PIX refund.
func TestCardRefundGoesBackThroughTheAcquirer(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	p := cardOf(t, c.do(postPayment(t, key, "order-123", cardBody(cardsim.TokenApproved)), true))

	r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+p.ID, "r1", usdBody(1000)), true))
	if r.Status != "succeeded" || r.PayerAmount.Value != 5075 {
		t.Errorf("refund = %+v, want succeeded with R$ 50.75 back", r)
	}
	if c.cardProvider.refunds.Load() != 1 || c.provider.refunds.Load() != 0 {
		t.Errorf("refunds asked of the acquirer %d and of PIX %d, want 1 and 0", c.cardProvider.refunds.Load(), c.provider.refunds.Load())
	}
	c.checkLedger(1000, 5075)
	if got := c.paymentStatus(); got != "partially_refunded" {
		t.Errorf("payment = %s, want partially_refunded", got)
	}
}
