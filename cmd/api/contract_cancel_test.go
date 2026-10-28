package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

func cancelRequest(t *testing.T, key, path string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost:8080"+path+"/cancel", http.NoBody)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

func (c *contract) statusOf(publicID string) (status string, version int) {
	c.t.Helper()
	id, err := payments.ParsePaymentID(publicID)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.pool.QueryRow(c.t.Context(), "SELECT status, version FROM payments WHERE id = $1", id).Scan(&status, &version); err != nil {
		c.t.Fatal(err)
	}
	return status, version
}

func TestCancelPendingPayment(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id, _ := c.createPayment(key)

	for range 2 {
		rec := c.do(cancelRequest(t, key, "/v1/payments/"+id), true)
		if rec.Code != http.StatusOK {
			t.Fatalf("cancel: status = %d: %s", rec.Code, rec.Body)
		}
		get := c.do(getRequest(t, key, "/v1/payments/"+id), true)
		if rec.Body.String() != get.Body.String() {
			t.Errorf("cancel answered %s, GET answers %s", rec.Body, get.Body)
		}
	}
	// checkEvent also wants version 2: the second cancel changed nothing.
	c.checkEvent("payment.canceled", key, id, "canceled")
	var events int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM outbox_events").Scan(&events); err != nil || events != 1 {
		t.Errorf("%d outbox events (%v), want 1", events, err)
	}
}

// The payer paid and the news is still on its way: the cancel finds the payment at the
// provider, confirms it and refuses to cancel.
func TestCancelConfirmsWhatThePayerAlreadyPaid(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id, _ := c.createPayment(key)
	if _, err := c.sim.Pay(t.Context(), c.attemptOf(id).externalID); err != nil {
		t.Fatal(err)
	}

	rec := c.do(cancelRequest(t, key, "/v1/payments/"+id), true)
	if code := problemCode(t, rec); rec.Code != http.StatusConflict || code != "payment_not_cancelable" {
		t.Errorf("cancel: got %d %s, want 409 payment_not_cancelable", rec.Code, code)
	}
	if status, _ := c.statusOf(id); status != "paid" {
		t.Errorf("payment is %s, want paid", status)
	}
}

// Decision 17's rule: no answer from the provider, no decision.
func TestCancelWithTheProviderDownChangesNothing(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id, _ := c.createPayment(key)
	if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpGet, Kind: pixsim.FailBefore, Count: 1}); err != nil {
		t.Fatal(err)
	}

	rec := c.do(cancelRequest(t, key, "/v1/payments/"+id), true)
	if code := problemCode(t, rec); rec.Code != http.StatusServiceUnavailable || code != "provider_unavailable" {
		t.Errorf("cancel: got %d %s, want 503 provider_unavailable", rec.Code, code)
	}
	if status, version := c.statusOf(id); status != "pending" || version != 1 {
		t.Errorf("payment is %s at version %d, want pending at 1", status, version)
	}
	if rec := c.do(cancelRequest(t, key, "/v1/payments/"+id), true); rec.Code != http.StatusOK {
		t.Errorf("retry: status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

// Money sent after the cancel is late money (decision 8): held for review, and the
// payment stays canceled.
func TestPaidAfterCancelIsHeldForReview(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id, _ := c.createPayment(key)
	if rec := c.do(cancelRequest(t, key, "/v1/payments/"+id), true); rec.Code != http.StatusOK {
		t.Fatalf("cancel: status = %d: %s", rec.Code, rec.Body)
	}

	a := c.attemptOf(id)
	paid, err := c.sim.Pay(t.Context(), a.externalID)
	if err != nil {
		t.Fatal(err)
	}
	store := paymentsadapters.NewPostgres(c.pool, paymentSinks())
	u := payments.AttemptUpdate{Event: payments.Succeed, ExternalID: paid.ID, BRCode: paid.BRCode, PaidAt: *paid.PaidAt, Amount: paid.Amount}
	if _, _, err := store.ApplyAttempt(t.Context(), a.id, &u, c.clock.Now()); err != nil {
		t.Fatal(err)
	}

	if status, _ := c.statusOf(id); status != "canceled" {
		t.Errorf("payment is %s, want canceled", status)
	}
	var kind string
	if err := c.pool.QueryRow(t.Context(), "SELECT kind FROM review_cases").Scan(&kind); err != nil || kind != "late_payment" {
		t.Errorf("review case %q (%v), want late_payment", kind, err)
	}
}

func TestCancelRefused(t *testing.T) {
	c := newContract(t)
	owner, other := c.merchantKey(), c.merchantKey()
	id, _ := c.createPayment(owner)
	if _, err := c.pool.Exec(t.Context(), "UPDATE payments SET status = 'expired', expiry_check_at = NULL"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, key, path string
		validRequest    bool
		wantStatus      int
		wantCode        string
	}{
		{"not pending", owner, "/v1/payments/" + id, true, http.StatusConflict, "payment_not_cancelable"},
		{"another merchant's payment", other, "/v1/payments/" + id, true, http.StatusNotFound, "payment_not_found"},
		{"malformed ID", owner, "/v1/payments/pay_nope", false, http.StatusNotFound, "payment_not_found"},
		{"no API key", "", "/v1/payments/" + id, false, http.StatusUnauthorized, "unauthorized"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.t = t
			rec := c.do(cancelRequest(t, tt.key, tt.path), tt.validRequest)
			if code := problemCode(t, rec); rec.Code != tt.wantStatus || code != tt.wantCode {
				t.Errorf("got %d %s, want %d %s", rec.Code, code, tt.wantStatus, tt.wantCode)
			}
		})
	}
}
