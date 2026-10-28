package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	idempotencyadapters "github.com/gengibrepower/cross-border-pay/internal/idempotency/adapters"
	idempotencyapp "github.com/gengibrepower/cross-border-pay/internal/idempotency/app"
	idempotency "github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	merchantsadapters "github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	merchantsapp "github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

func postRefund(t *testing.T, key, path, idempotencyKey, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost:8080"+path+"/refunds", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return req
}

func usdBody(cents int64) string {
	return fmt.Sprintf(`{"amount": {"value": %d, "currency": "USD"}}`, cents)
}

// paidPayment is the GameStore's US$ 20.00 paid through the simulator, so a refund has
// a charge to return money from.
func (c *contract) paidPayment(key string) string {
	c.t.Helper()
	id, _ := c.createPayment(key)
	a := c.attemptOf(id)
	paid, err := c.sim.Pay(c.t.Context(), a.externalID)
	if err != nil {
		c.t.Fatal(err)
	}
	store := paymentsadapters.NewPostgres(c.pool, paymentSinks())
	u := payments.AttemptUpdate{Event: payments.Succeed, ExternalID: paid.ID, BRCode: paid.BRCode, PaidAt: *paid.PaidAt, Amount: paid.Amount}
	if _, _, err := store.ApplyAttempt(c.t.Context(), a.id, &u, c.clock.Now()); err != nil {
		c.t.Fatal(err)
	}
	return id
}

type refundBody struct {
	ID          string `json:"id"`
	PaymentID   string `json:"payment_id"`
	Status      string `json:"status"`
	PayerAmount struct {
		Value int64 `json:"value"`
	} `json:"payer_amount"`
}

func refundOf(t *testing.T, rec *httptest.ResponseRecorder) refundBody {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("refund: status = %d: %s", rec.Code, rec.Body)
	}
	var r refundBody
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// balance is the sum of the entries on the accounts whose code starts with prefix.
func (c *contract) balance(prefix string) int64 {
	c.t.Helper()
	var n int64
	err := c.pool.QueryRow(c.t.Context(), `SELECT coalesce(sum(e.amount), 0)::bigint FROM ledger_entries AS e
		JOIN ledger_accounts AS a ON a.id = e.account_id WHERE a.code LIKE $1 || '%'`, prefix).Scan(&n)
	if err != nil {
		c.t.Fatal(err)
	}
	return n
}

// checkLedger is every refund's effect on the books, from what the payment took in.
func (c *contract) checkLedger(refunded, payerRefunded int64) {
	c.t.Helper()
	for _, b := range []struct {
		account string
		want    int64
	}{
		{"refunds_in_flight:", 0},
		{"merchant_payable:", 1940 - refunded},
		{"provider_receivables:", 10150 - payerRefunded},
	} {
		if got := c.balance(b.account); got != b.want {
			c.t.Errorf("%s balance = %d, want %d", b.account, got, b.want)
		}
	}
	var unbalanced int
	err := c.pool.QueryRow(c.t.Context(), `SELECT count(*) FROM (SELECT currency FROM ledger_entries
		GROUP BY currency HAVING sum(amount) <> 0) AS s`).Scan(&unbalanced)
	if err != nil || unbalanced != 0 {
		c.t.Errorf("%d currencies do not sum to zero (%v)", unbalanced, err)
	}
}

func (c *contract) checkRefundEvent(t, refundID string) {
	c.t.Helper()
	var payload []byte
	err := c.pool.QueryRow(c.t.Context(), "SELECT payload FROM outbox_events WHERE type = $1 AND convert_from(payload, 'UTF8') LIKE '%' || $2 || '%'",
		t, refundID).Scan(&payload)
	if err != nil {
		c.t.Fatalf("no %s for %s in the outbox: %v", t, refundID, err)
	}
	var event any
	if err := json.Unmarshal(payload, &event); err != nil {
		c.t.Fatal(err)
	}
	if err := c.doc.Components.Schemas["RefundEvent"].Value.VisitJSON(event); err != nil {
		c.t.Errorf("%s does not match RefundEvent: %v\n%s", t, err, payload)
	}
}

// The worked example of F7: two halves of the GameStore, each R$ 50.75 to the payer;
// the payment ends refunded and the merchant owes back the US$ 0.60 fee.
func TestRefundInTwoHalves(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)

	first := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(1000)), true))
	if first.Status != "succeeded" || first.PayerAmount.Value != 5075 || first.PaymentID != id {
		t.Fatalf("first refund = %+v, want succeeded with R$ 50.75 back", first)
	}
	if status, version := c.statusOf(id); status != "partially_refunded" || version != 3 {
		t.Errorf("payment is %s at version %d, want partially_refunded at 3", status, version)
	}
	c.checkRefundEvent("refund.succeeded", first.ID)

	second := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r2", usdBody(1000)), true))
	if second.Status != "succeeded" || second.PayerAmount.Value != 5075 {
		t.Fatalf("second refund = %+v, want succeeded with R$ 50.75 back", second)
	}
	if status, version := c.statusOf(id); status != "refunded" || version != 4 {
		t.Errorf("payment is %s at version %d, want refunded at 4", status, version)
	}
	c.checkLedger(2000, 10150)

	rec := c.do(postRefund(t, key, "/v1/payments/"+id, "r3", usdBody(1)), true)
	if code := problemCode(t, rec); rec.Code != http.StatusConflict || code != "payment_not_refundable" {
		t.Errorf("refund of a refunded payment: got %d %s, want 409 payment_not_refundable", rec.Code, code)
	}

	// The events listing shows refund events through the same contract.
	if rec := c.do(getRequest(t, key, "/v1/events"), true); rec.Code != http.StatusOK {
		t.Errorf("GET /v1/events: %d %s", rec.Code, rec.Body)
	}
}

func TestRefundRetryReplays(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)

	first := c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(500)), true)
	again := c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(500)), true)
	if again.Code != http.StatusCreated || again.Body.String() != first.Body.String() || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("retry = %d %s, want the first 201 replayed: %s", again.Code, again.Body, first.Body)
	}
	if n := c.count("refunds"); n != 1 {
		t.Errorf("%d refunds, want 1", n)
	}
}

// A refusal is an outcome: 201 failed, the hold back with the merchant, the payment
// still paid, and refund.failed on the webhook.
func TestRefundRefusedByTheProvider(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.Reject, Count: 1}); err != nil {
		t.Fatal(err)
	}

	r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(1000)), true))
	if r.Status != "failed" {
		t.Fatalf("refund = %+v, want failed", r)
	}
	if status, version := c.statusOf(id); status != "paid" || version != 2 {
		t.Errorf("payment is %s at version %d, want paid at 2", status, version)
	}
	c.checkRefundEvent("refund.failed", r.ID)
	c.checkLedger(0, 0)

	// The refused refund no longer counts: the whole payment can still be refunded.
	if r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r2", usdBody(2000)), true)); r.Status != "succeeded" {
		t.Errorf("full refund after the refusal = %+v, want succeeded", r)
	}
}

// Section 4.3: the provider may have returned the money, so the refund waits in
// unknown, the hold kept, and nothing is posted as gone.
func TestRefundWithoutAnAnswerStaysPending(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.TimeoutAfter, Count: 1}); err != nil {
		t.Fatal(err)
	}

	r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(1000)), true))
	if r.Status != "pending" {
		t.Fatalf("refund = %+v, want pending", r)
	}
	var status string
	if err := c.pool.QueryRow(t.Context(), "SELECT status FROM refunds").Scan(&status); err != nil || status != "unknown" {
		t.Errorf("refund status = %s (%v), want unknown", status, err)
	}
	if got := c.balance("refunds_in_flight:"); got != 1000 {
		t.Errorf("in flight = %d, want the 1000 held", got)
	}
	if got := c.balance("merchant_payable:"); got != 940 {
		t.Errorf("merchant payable = %d, want 940", got)
	}
}

func TestRefundRefused(t *testing.T) {
	c := newContract(t)
	owner, other := c.merchantKey(), c.merchantKey()
	paid := c.paidPayment(owner)
	pending, _ := c.createPayment(owner)

	tests := []struct {
		name, key, path, body string
		validRequest          bool
		wantStatus            int
		wantCode              string
	}{
		{"more than paid", owner, "/v1/payments/" + paid, usdBody(2001), true, http.StatusUnprocessableEntity, "refund_exceeds_payment"},
		{"not paid", owner, "/v1/payments/" + pending, usdBody(100), true, http.StatusConflict, "payment_not_refundable"},
		{"another merchant's payment", other, "/v1/payments/" + paid, usdBody(100), true, http.StatusNotFound, "payment_not_found"},
		{"zero", owner, "/v1/payments/" + paid, usdBody(0), true, http.StatusUnprocessableEntity, "invalid_amount"},
		{"in reais", owner, "/v1/payments/" + paid, `{"amount": {"value": 100, "currency": "BRL"}}`, true, http.StatusUnprocessableEntity, "unsupported_currency"},
		{"decimal", owner, "/v1/payments/" + paid, `{"amount": {"value": 10.5, "currency": "USD"}}`, false, http.StatusBadRequest, "invalid_amount"},
		{"no amount", owner, "/v1/payments/" + paid, `{}`, false, http.StatusBadRequest, "invalid_request"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.t = t
			rec := c.do(postRefund(t, tt.key, tt.path, fmt.Sprintf("k%d", i), tt.body), tt.validRequest)
			if code := problemCode(t, rec); rec.Code != tt.wantStatus || code != tt.wantCode {
				t.Errorf("got %d %s, want %d %s", rec.Code, code, tt.wantStatus, tt.wantCode)
			}
		})
	}
	c.t = t
	if n := c.count("refunds"); n != 0 {
		t.Errorf("%d refunds stored, want 0", n)
	}
}

// The README's cited test 2 (invariant 8): concurrent partial refunds of random amounts
// never pass what was paid. Every refund the gateway accepted, the provider completed:
// the gateway's own check refused first, so the simulator's never had to. Every refusal
// was right: it would still not fit in what is left at the end.
func TestConcurrentPartialRefundsNeverExceedThePayment(t *testing.T) {
	const rounds, requests = 5, 40
	c := newContract(t)
	key := c.merchantKey()
	var refunded, payerRefunded int64
	for p := range rounds {
		id := c.paidPayment(key)
		rng := mrand.New(mrand.NewPCG(uint64(p), 1)) //nolint:gosec // G404: reproducible amounts, nothing secret.
		amounts := make([]int64, requests)
		for i := range amounts {
			amounts[i] = 1 + rng.Int64N(300)
		}

		type outcome struct {
			amount int64
			code   int
			body   string
		}
		results := make([]outcome, requests)
		var wg sync.WaitGroup
		for i, amount := range amounts {
			wg.Go(func() {
				rec := httptest.NewRecorder()
				c.handler.ServeHTTP(rec, postRefund(t, key, "/v1/payments/"+id, rand.Text(), usdBody(amount)))
				results[i] = outcome{amount: amount, code: rec.Code, body: rec.Body.String()}
			})
		}
		wg.Wait()

		var sum, payer int64
		var refused []int64
		for _, r := range results {
			switch {
			case r.code == http.StatusCreated:
				var b refundBody
				if err := json.Unmarshal([]byte(r.body), &b); err != nil {
					t.Fatal(err)
				}
				if b.Status != "succeeded" {
					t.Fatalf("refund of %d = %s, want succeeded: the provider refused what the gateway accepted", r.amount, r.body)
				}
				sum += r.amount
				payer += b.PayerAmount.Value
			case r.code == http.StatusUnprocessableEntity && strings.Contains(r.body, "refund_exceeds_payment"):
				refused = append(refused, r.amount)
			case r.code == http.StatusConflict && strings.Contains(r.body, "payment_not_refundable"):
				refused = append(refused, r.amount)
			default:
				t.Fatalf("refund of %d: %d %s", r.amount, r.code, r.body)
			}
		}
		if sum > 2000 || payer > 10150 {
			t.Fatalf("payment %d: refunded %d of 2000, R$ %d of 10150", p, sum, payer)
		}
		for _, x := range refused {
			if sum+x <= 2000 {
				t.Errorf("payment %d: a refund of %d was refused with %d refunded of 2000", p, x, sum)
			}
		}
		want := "partially_refunded"
		if sum == 2000 {
			want = "refunded"
		}
		if status, _ := c.statusOf(id); status != want {
			t.Errorf("payment %d is %s after %d refunded, want %s", p, status, sum, want)
		}
		t.Logf("payment %d: %d refunds accepted for %d of 2000, %d refused", p, requests-len(refused), sum, len(refused))
		refunded += sum
		payerRefunded += payer
	}
	var failed int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM refunds WHERE status <> 'succeeded'").Scan(&failed); err != nil || failed != 0 {
		t.Errorf("%d refunds did not succeed (%v), want 0", failed, err)
	}
	for _, b := range []struct {
		account string
		want    int64
	}{
		{"refunds_in_flight:", 0},
		{"merchant_payable:", rounds*1940 - refunded},
		{"provider_receivables:", rounds*10150 - payerRefunded},
	} {
		if got := c.balance(b.account); got != b.want {
			t.Errorf("%s balance = %d, want %d", b.account, got, b.want)
		}
	}
}

// crashedRefund is a refund request that held the key and committed its first
// transaction, the refund in created with its hold, and then died.
func (c *contract) crashedRefund(merchantKey, paymentID, idempotencyKey string, cents int64) (*paymentsadapters.Postgres, payments.Refund) {
	c.t.Helper()
	ctx := c.t.Context()
	principal, err := merchantsapp.NewService(merchantsadapters.NewPostgres(c.pool), rand.Reader).Authenticate(ctx, merchantKey)
	if err != nil {
		c.t.Fatal(err)
	}
	keys := idempotencyapp.NewService(idempotencyadapters.NewPostgres(c.pool), c.clock, rand.Reader)
	req, err := idempotency.NewRequest(idempotency.MerchantID(principal.MerchantID), idempotencyKey,
		idempotency.HashRequest(http.MethodPost, "/v1/payments/"+paymentID+"/refunds", []byte(usdBody(cents))))
	if err != nil {
		c.t.Fatal(err)
	}
	acquired, err := keys.Begin(ctx, &req)
	if err != nil {
		c.t.Fatal(err)
	}
	id, err := payments.ParsePaymentID(paymentID)
	if err != nil {
		c.t.Fatal(err)
	}
	store := paymentsadapters.NewPostgres(c.pool, paymentSinks())
	r, err := store.CreateRefund(ctx, acquired.Held, payments.MerchantID(principal.MerchantID), id, money.New(cents, money.USD), c.provider.Name(), c.clock.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	return store, r
}

func TestResumedRefundFromCreatedCallsTheProvider(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	_, crashed := c.crashedRefund(key, id, "r1", 1000)

	c.clock.Advance(idempotency.Lease)
	r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(1000)), true))
	if r.ID != crashed.ID().String() || r.Status != "succeeded" {
		t.Fatalf("resumed refund = %+v, want %s succeeded", r, crashed.ID())
	}
	if n := c.provider.refunds.Load(); n != 1 || c.count("refunds") != 1 {
		t.Errorf("provider called %d times for %d refunds, want 1 and 1", n, c.count("refunds"))
	}
	c.checkLedger(1000, 5075)
}

// Decision 17 for refunds: sent means the money may be gone, so the resumed request
// never asks again blind; the refund waits in unknown, its hold kept.
func TestResumedRefundFromSentDoesNotCallAgain(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.paidPayment(key)
	store, crashed := c.crashedRefund(key, id, "r1", 1000)
	rate, err := fx.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ApplyRefund(t.Context(), crashed.ID(), &payments.RefundUpdate{Event: payments.SendRefund}, rate, c.clock.Now()); err != nil {
		t.Fatal(err)
	}

	c.clock.Advance(idempotency.Lease)
	r := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(1000)), true))
	if r.Status != "pending" || c.provider.refunds.Load() != 0 {
		t.Errorf("resumed refund = %+v after %d calls, want pending after none", r, c.provider.refunds.Load())
	}
	if got := c.balance("refunds_in_flight:"); got != 1000 {
		t.Errorf("in flight = %d, want the 1000 held", got)
	}
}
