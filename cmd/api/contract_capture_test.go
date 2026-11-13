package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
)

func manualCardBody(token string) string {
	return fmt.Sprintf(`{"amount": {"value": 2000, "currency": "USD"}, "method": "card", "reference": "order-123", "card": {"token": %q, "capture_method": "manual"}}`, token)
}

func captureRequest(t *testing.T, key, id, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost:8080/v1/payments/"+id+"/capture", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

// authorized is a manual card payment of the GameStore's US$ 20.00, authorized only.
func (c *contract) authorized(key string) cardPayment {
	c.t.Helper()
	p := cardOf(c.t, c.do(postPayment(c.t, key, "order-123", manualCardBody(cardsim.TokenApproved)), true))
	if p.Status != "pending" || p.Card.Status != "authorized" || p.Card.CaptureMethod != "manual" {
		c.t.Fatalf("manual card = %+v, want pending, authorized and manual", p)
	}
	return p
}

// checkBooks is the confirmation's effect on the merchant's payable and the provider's
// receivables, less what refunds took.
func (c *contract) checkBooks(net, payer int64) {
	c.t.Helper()
	for _, b := range []struct {
		account string
		want    int64
	}{
		{"merchant_payable:", net},
		{"provider_receivables:", payer},
		{"refunds_in_flight:", 0},
	} {
		if got := c.balance(b.account); got != b.want {
			c.t.Errorf("%s balance = %d, want %d", b.account, got, b.want)
		}
	}
}

// Decision 18: authorized now, captured later. Nothing is posted while the card is
// only held, and the capture posts what a card captured at once would.
func TestManualCardIsCapturedWhole(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	p := c.authorized(key)
	if !p.Quote.ExpiresAt.Equal(t0.Add(payments.CardAuthorizationValidity)) {
		t.Errorf("quote expires %v, want the authorization's 7 days", p.Quote.ExpiresAt)
	}
	if n := c.count("ledger_entries"); n != 0 {
		t.Errorf("%d ledger entries for a hold", n)
	}

	got := cardOf(t, c.do(captureRequest(t, key, p.ID, ""), true))
	if got.Status != "paid" || got.Card.Status != "captured" || got.Card.Captured == nil || got.Card.Captured.Amount.Value != 2000 ||
		got.Card.Captured.PayerAmount.Value != 10150 || got.Card.Captured.NetAmount.Value != 1940 {
		t.Fatalf("captured = %+v %+v, want paid with the whole quote", got, got.Card.Captured)
	}
	c.checkBooks(1940, 10150)
	if again := c.do(captureRequest(t, key, p.ID, `{}`), true); again.Code != http.StatusOK || cardOf(t, again).Status != "paid" {
		t.Errorf("the same capture again = %d %s, want 200 paid", again.Code, again.Body)
	}
	if other := c.do(captureRequest(t, key, p.ID, `{"amount": {"value": 1000, "currency": "USD"}}`), true); other.Code != http.StatusConflict || problemCode(t, other) != "capture_amount_conflict" {
		t.Errorf("a capture of another amount = %d %s, want 409 capture_amount_conflict", other.Code, other.Body)
	}
	c.checkBooks(1940, 10150)
	if n := c.count("outbox_events WHERE type = 'payment.paid'"); n != 1 {
		t.Errorf("%d payment.paid events, want 1", n)
	}
}

// The GameStore ships US$ 15.00 of the US$ 20.00 it authorized: the payer pays
// R$ 76.12, the fee is on the US$ 15.00, the rest of the hold is released, and refunds
// stop at what was captured (invariant 8).
func TestPartialCapture(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	p := c.authorized(key)

	got := cardOf(t, c.do(captureRequest(t, key, p.ID, `{"amount": {"value": 1500, "currency": "USD"}}`), true))
	captured := got.Card.Captured
	if got.Status != "paid" || captured == nil || captured.Amount.Value != 1500 || captured.PayerAmount.Value != 7612 ||
		captured.Fee.Value != 45 || captured.NetAmount.Value != 1455 {
		t.Fatalf("partial capture = %+v %+v, want US$ 15.00: R$ 76.12, fee US$ 0.45, net US$ 14.55", got, captured)
	}
	if again := cardOf(t, c.do(getPayment(t, key, p.ID), true)); again.Card.Captured.Amount.Value != 1500 {
		t.Errorf("GET shows %d captured, want 1500", again.Card.Captured.Amount.Value)
	}
	at, err := c.card.Get(t.Context(), c.attemptOf(p.ID).id.String())
	if err != nil || at.Captured != money.New(7612, money.BRL) {
		t.Errorf("acquirer captured %s (%v), want R$ 76.12", at.Captured, err)
	}
	c.checkBooks(1455, 7612)
	// The balance waits on the net of what was captured, not of what was authorized.
	if b := c.getBalance(key); b.Pending.Value != 1455 || b.Available.Value != 0 {
		t.Errorf("balance = %+v, want US$ 14.55 pending", b)
	}

	over := c.do(postRefund(t, key, "/v1/payments/"+p.ID, "r-over", usdBody(1501)), true)
	if over.Code != http.StatusUnprocessableEntity || problemCode(t, over) != "refund_exceeds_payment" {
		t.Errorf("refund of more than was captured = %d %s, want 422 refund_exceeds_payment", over.Code, over.Body)
	}
	r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+p.ID, "r-all", usdBody(1500)), true))
	if r.Status != "succeeded" || r.PayerAmount.Value != 7612 {
		t.Errorf("refund of all that was captured = %+v, want R$ 76.12 back", r)
	}
	if status := c.paymentStatus(); status != "refunded" {
		t.Errorf("payment = %s, want refunded", status)
	}
	// The merchant gave back the US$ 15.00 and keeps owing the fee, as with PIX.
	c.checkBooks(1455-1500, 0)
}

// A capture whose answer is lost leaves the payment processing; it is never captured
// again in the dark, and the resolver confirms it for the amount the capture stored.
func TestLostCaptureIsConfirmedByTheResolver(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	p := c.authorized(key)
	if err := c.card.InjectFault(t.Context(), cardsim.Fault{Op: cardsim.OpCapture, Kind: cardsim.TimeoutAfter, Count: 1}); err != nil {
		t.Fatal(err)
	}
	body := `{"amount": {"value": 1500, "currency": "USD"}}`
	got := cardOf(t, c.do(captureRequest(t, key, p.ID, body), true))
	if got.Status != "pending" || got.Card.Status != "processing" {
		t.Fatalf("lost capture = %+v, want pending and processing", got)
	}
	if again := c.do(captureRequest(t, key, p.ID, body), true); again.Code != http.StatusConflict || problemCode(t, again) != "capture_in_progress" {
		t.Errorf("capture while unknown = %d %s, want 409 capture_in_progress", again.Code, again.Body)
	}

	resolver := paymentsapp.NewResolver(paymentsadapters.NewPostgres(c.pool, paymentSinks()), c.deps.providers, c.clock,
		slog.New(slog.DiscardHandler), providerTimeout)
	c.clock.Advance(payments.CheckBase)
	if n, err := resolver.Resolve(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("Resolve() = %d, %v; want 1 claimed", n, err)
	}
	got = cardOf(t, c.do(getPayment(t, key, p.ID), true))
	if got.Status != "paid" || got.Card.Captured == nil || got.Card.Captured.Amount.Value != 1500 {
		t.Errorf("after the resolver = %+v, want paid for US$ 15.00", got)
	}
	c.checkBooks(1455, 7612)
}

// A capture that failed before the acquirer acted is found not captured; the merchant
// may try again, but only the same capture: the acquirer could still apply the first.
func TestUncertainCaptureIsRetriedOnlyForTheSameAmount(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	p := c.authorized(key)
	if err := c.card.InjectFault(t.Context(), cardsim.Fault{Op: cardsim.OpCapture, Kind: cardsim.FailBefore, Count: 1}); err != nil {
		t.Fatal(err)
	}
	body := `{"amount": {"value": 1500, "currency": "USD"}}`
	if got := cardOf(t, c.do(captureRequest(t, key, p.ID, body), true)); got.Card.Status != "processing" {
		t.Fatalf("failed capture = %+v, want processing", got)
	}
	resolver := paymentsapp.NewResolver(paymentsadapters.NewPostgres(c.pool, paymentSinks()), c.deps.providers, c.clock,
		slog.New(slog.DiscardHandler), providerTimeout)
	c.clock.Advance(payments.CheckBase)
	if n, err := resolver.Resolve(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("Resolve() = %d, %v; want 1 claimed", n, err)
	}
	if got := cardOf(t, c.do(getPayment(t, key, p.ID), true)); got.Status != "pending" || got.Card.Status != "authorized" {
		t.Fatalf("after the resolver = %+v, want pending and authorized again", got)
	}
	if rec := c.do(captureRequest(t, key, p.ID, `{"amount": {"value": 2000, "currency": "USD"}}`), true); rec.Code != http.StatusConflict || problemCode(t, rec) != "capture_amount_conflict" {
		t.Errorf("another amount after an uncertain capture = %d %s, want 409 capture_amount_conflict", rec.Code, rec.Body)
	}
	got := cardOf(t, c.do(captureRequest(t, key, p.ID, ""), true))
	if got.Status != "paid" || got.Card.Captured.Amount.Value != 1500 {
		t.Errorf("the capture again with no amount = %+v, want paid for the US$ 15.00 started", got)
	}
	c.checkBooks(1455, 7612)
}

// Canceling an authorized payment voids the hold at once instead of leaving the
// payer's limit taken for 7 days.
func TestCancelVoidsTheAuthorization(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	p := c.authorized(key)
	got := cardOf(t, c.do(cancelRequest(t, key, "/v1/payments/"+p.ID), true))
	if got.Status != "canceled" || got.Card.Status != "released" {
		t.Errorf("canceled = %+v, want canceled and released", got)
	}
	at, err := c.card.Get(t.Context(), c.attemptOf(p.ID).id.String())
	if err != nil || at.Status != cardsim.Voided {
		t.Errorf("authorization at the acquirer = %s (%v), want voided", at.Status, err)
	}
	if rec := c.do(captureRequest(t, key, p.ID, ""), true); rec.Code != http.StatusConflict || problemCode(t, rec) != "payment_not_capturable" {
		t.Errorf("capture after cancel = %d %s, want 409 payment_not_capturable", rec.Code, rec.Body)
	}
}

// An authorization nobody captured expires with its quote, and then nothing captures it.
func TestUncapturedAuthorizationExpires(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	p := c.authorized(key)
	c.clock.Advance(payments.CardAuthorizationValidity)
	if rec := c.do(captureRequest(t, key, p.ID, ""), true); rec.Code != http.StatusConflict || problemCode(t, rec) != "authorization_expired" {
		t.Errorf("capture at the deadline = %d %s, want 409 authorization_expired", rec.Code, rec.Body)
	}
	expirer := paymentsapp.NewExpirer(paymentsadapters.NewPostgres(c.pool, paymentSinks()), c.deps.providers, c.clock,
		slog.New(slog.DiscardHandler), providerTimeout)
	if n, err := expirer.Expire(t.Context(), paymentsapp.ExpiryBatch); err != nil || n != 1 {
		t.Fatalf("Expire() = %d, %v; want 1", n, err)
	}
	got := cardOf(t, c.do(getPayment(t, key, p.ID), true))
	if got.Status != "expired" || got.Card.Status != "released" {
		t.Errorf("after expiry = %+v, want expired and released", got)
	}
	if n := c.count("ledger_entries"); n != 0 {
		t.Errorf("%d ledger entries for an authorization never captured", n)
	}
}

func TestCaptureErrors(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	manual := c.authorized(key)
	pix, _ := c.createPayment(key)
	auto := cardOf(t, c.do(postPayment(t, key, "auto", cardBody(cardsim.TokenApproved)), true))
	declined := cardOf(t, c.do(postPayment(t, key, "declined", manualCardBody(cardsim.TokenDeclined)), true))
	for _, tc := range []struct {
		name, id, body string
		validRequest   bool
		status         int
		code           string
	}{
		{"more than authorized", manual.ID, `{"amount": {"value": 2001, "currency": "USD"}}`, true, http.StatusUnprocessableEntity, "capture_exceeds_authorization"},
		{"zero", manual.ID, `{"amount": {"value": 0, "currency": "USD"}}`, true, http.StatusUnprocessableEntity, "invalid_amount"},
		{"in BRL", manual.ID, `{"amount": {"value": 1000, "currency": "BRL"}}`, true, http.StatusUnprocessableEntity, "invalid_amount"},
		{"unknown field", manual.ID, `{"amount": {"value": 1000, "currency": "USD"}, "final": true}`, false, http.StatusBadRequest, "invalid_request"},
		{"a PIX payment", pix, "", true, http.StatusConflict, "payment_not_capturable"},
		{"a declined card", declined.ID, "", true, http.StatusConflict, "payment_not_capturable"},
		{"another amount of a card captured at once", auto.ID, `{"amount": {"value": 1000, "currency": "USD"}}`, true, http.StatusConflict, "capture_amount_conflict"},
		{"no such payment", "pay_06f1z4ghq3m5ttqb0gnsx4yy3c", "", true, http.StatusNotFound, "payment_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.t = t
			rec := c.do(captureRequest(t, key, tc.id, tc.body), tc.validRequest)
			if rec.Code != tc.status || problemCode(t, rec) != tc.code {
				t.Errorf("got %d %q, want %d %q: %s", rec.Code, problemCode(t, rec), tc.status, tc.code, rec.Body)
			}
		})
	}
	c.t = t
	if rec := c.do(captureRequest(t, key, auto.ID, ""), true); rec.Code != http.StatusOK {
		t.Errorf("capture of a card captured at once = %d %s, want 200 as it is", rec.Code, rec.Body)
	}
	if n := c.count("ledger_entries WHERE true"); n == 0 {
		t.Error("the card captured at once posted nothing")
	}
	if got := cardOf(t, c.do(getPayment(t, key, manual.ID), true)); got.Status != "pending" || got.Card.Status != "authorized" {
		t.Errorf("after refused captures = %+v, want still pending and authorized", got)
	}
}
