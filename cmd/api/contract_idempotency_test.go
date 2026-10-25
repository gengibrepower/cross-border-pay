package main

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	fxadapters "github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	fxapp "github.com/gengibrepower/cross-border-pay/internal/fx/app"
	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	idempotencyadapters "github.com/gengibrepower/cross-border-pay/internal/idempotency/adapters"
	idempotencyapp "github.com/gengibrepower/cross-border-pay/internal/idempotency/app"
	idempotency "github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	merchantsadapters "github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	merchantsapp "github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

func (c *contract) count(table string) int {
	c.t.Helper()
	var n int
	if err := c.pool.QueryRow(c.t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

func paymentID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// Section 4.1, step 4: a retry after a timeout gets the stored answer and creates nothing.
func TestRetryReplaysTheCreatedPayment(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	first := c.do(postPayment(t, key, "order-123", gameStore), true)
	retry := c.do(postPayment(t, key, "order-123", gameStore), true)

	if retry.Code != http.StatusCreated || retry.Body.String() != first.Body.String() ||
		retry.Header().Get("Location") != first.Header().Get("Location") {
		t.Errorf("retry got %d %s, want the first 201 %s", retry.Code, retry.Body, first.Body)
	}
	if retry.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("retry is not marked Idempotent-Replayed")
	}
	if retry.Header().Get("X-Request-Id") == first.Header().Get("X-Request-Id") {
		t.Error("retry repeats the first X-Request-Id; the header must be the current request's")
	}
	if n := c.count("payments"); n != 1 {
		t.Errorf("%d payments stored, want 1", n)
	}
}

func TestSameKeyWithAnotherPaymentIsRejected(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	c.do(postPayment(t, key, "k", gameStore), true)
	other := `{"amount": {"value": 3000, "currency": "USD"}, "method": "pix", "reference": "order-124"}`
	rec := c.do(postPayment(t, key, "k", other), true)
	if code := problemCode(t, rec); rec.Code != http.StatusUnprocessableEntity || code != "idempotency_key_reused" {
		t.Errorf("got %d %s, want 422 idempotency_key_reused", rec.Code, code)
	}
	if n := c.count("payments"); n != 1 {
		t.Errorf("%d payments stored, want 1", n)
	}
}

// Decision 16: a 4xx is stored, so the retry gets it again, marked as a replay, with
// the request_id of the request that was rejected.
func TestRetryReplaysAClientError(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	zero := `{"amount": {"value": 0, "currency": "USD"}, "method": "pix", "reference": "o"}`
	first := c.do(postPayment(t, key, "k", zero), true)
	retry := c.do(postPayment(t, key, "k", zero), true)
	if first.Code != http.StatusUnprocessableEntity || retry.Code != first.Code || retry.Body.String() != first.Body.String() {
		t.Errorf("got %d %s then %d %s, want the same 422 twice", first.Code, first.Body, retry.Code, retry.Body)
	}
	if retry.Header().Get("Idempotent-Replayed") != "true" || retry.Header().Get("Content-Type") != "application/problem+json" {
		t.Errorf("retry headers = %v, want a replayed problem", retry.Header())
	}
}

func TestKeysOfDifferentMerchantsAreIndependent(t *testing.T) {
	c := newContract(t)
	a := c.do(postPayment(t, c.merchantKey(), "order-123", gameStore), true)
	b := c.do(postPayment(t, c.merchantKey(), "order-123", gameStore), true)
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated || paymentID(t, a) == paymentID(t, b) {
		t.Errorf("got %d %s and %d %s, want two different payments", a.Code, a.Body, b.Code, b.Body)
	}
	if b.Header().Get("Idempotent-Replayed") != "" {
		t.Error("the second merchant got the first one's answer")
	}
}

// Many clients sending the same request at once create one payment. How many see 409
// and how many a replay depends on timing; none may see a second payment.
func TestConcurrentRetriesCreateOnePayment(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	const n = 10

	start := make(chan struct{})
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			req := postPayment(t, key, "order-123", gameStore)
			<-start
			recs[i] = httptest.NewRecorder()
			c.handler.ServeHTTP(recs[i], req)
		})
	}
	close(start)
	wg.Wait()

	ids := map[string]bool{}
	for i, rec := range recs {
		switch rec.Code {
		case http.StatusCreated:
			ids[paymentID(t, rec)] = true
		case http.StatusConflict:
			if code := problemCode(t, rec); code != "idempotency_key_in_progress" {
				t.Errorf("request %d: 409 %s, want idempotency_key_in_progress", i, code)
			}
		default:
			t.Errorf("request %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if len(ids) != 1 || c.count("payments") != 1 {
		t.Errorf("answers name %d payments and %d are stored, want 1 and 1", len(ids), c.count("payments"))
	}
}

// Section 4.2, case 4, the reason for recovery points: the request created the payment
// and died before storing its answer. The retry, once the lease is over, returns that
// same payment instead of creating a second one.
func TestRetryAfterACrashReturnsThePaymentAlreadyCreated(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	principal, err := merchantsapp.NewService(merchantsadapters.NewPostgres(c.pool), rand.Reader).Authenticate(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}

	// The request that dies: it holds the key and creates the payment, then nothing.
	keys := idempotencyapp.NewService(idempotencyadapters.NewPostgres(c.pool), c.clock, rand.Reader)
	req, err := idempotency.NewRequest(idempotency.MerchantID(principal.MerchantID), "order-123",
		idempotency.HashRequest(http.MethodPost, "/v1/payments", []byte(gameStore)))
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := keys.Begin(t.Context(), &req)
	if err != nil {
		t.Fatal(err)
	}
	rate, err := fx.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		t.Fatal(err)
	}
	quoter := fxapp.NewQuoter(fxadapters.NewFixedRates(rate), c.clock, fx.DefaultPricing)
	svc := paymentsapp.NewService(quoter, paymentsadapters.NewPostgres(c.pool))
	created, _, err := svc.Create(t.Context(), acquired.Held, payments.MerchantID(principal.MerchantID),
		"pix", money.New(2000, money.USD), "order-123")
	if err != nil {
		t.Fatal(err)
	}

	if rec := c.do(postPayment(t, key, "order-123", gameStore), true); rec.Code != http.StatusConflict {
		t.Fatalf("retry within the lease got %d %s, want 409", rec.Code, rec.Body)
	}

	c.clock.Advance(idempotency.Lease)
	retry := c.do(postPayment(t, key, "order-123", gameStore), true)
	if retry.Code != http.StatusCreated || paymentID(t, retry) != created.ID().String() {
		t.Fatalf("retry after the lease got %d %s, want 201 with %s", retry.Code, retry.Body, created.ID())
	}
	get := c.do(getRequest(t, key, "/v1/payments/"+created.ID().String()), true)
	if retry.Body.String() != get.Body.String() {
		t.Errorf("resumed answer differs from the stored payment:\n got %s\nwant %s", retry.Body, get.Body)
	}
	if p, q := c.count("payments"), c.count("quotes"); p != 1 || q != 1 {
		t.Errorf("%d payments and %d quotes stored, want 1 and 1", p, q)
	}

	// The resumed request completed the key, so the next retry is a plain replay.
	if again := c.do(postPayment(t, key, "order-123", gameStore), true); again.Header().Get("Idempotent-Replayed") != "true" || again.Body.String() != retry.Body.String() {
		t.Errorf("next retry got %d %s, want the replayed 201", again.Code, again.Body)
	}
}
