package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/brcode"
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
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

type pixField struct {
	Status string `json:"status"`
	BRCode string `json:"br_code"`
}

func pixOf(t *testing.T, rec *httptest.ResponseRecorder) pixField {
	t.Helper()
	var p struct {
		Pix *pixField `json:"pix"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Pix == nil {
		t.Fatalf("response has no pix: %s", rec.Body)
	}
	return *p.Pix
}

type storedAttempt struct {
	id         payments.AttemptID
	status     string
	externalID string
}

func (c *contract) attemptOf(publicID string) storedAttempt {
	c.t.Helper()
	id, err := payments.ParsePaymentID(publicID)
	if err != nil {
		c.t.Fatal(err)
	}
	var a storedAttempt
	err = c.pool.QueryRow(c.t.Context(),
		"SELECT id, status, coalesce(external_id, '') FROM payment_attempts WHERE payment_id = $1", id).
		Scan(&a.id, &a.status, &a.externalID)
	if err != nil {
		c.t.Fatal(err)
	}
	return a
}

func TestCreatePaymentReturnsAPayableBRCode(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	rec := c.do(postPayment(t, key, "order-123", gameStore), true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	pix := pixOf(t, rec)
	code, err := brcode.Parse(pix.BRCode)
	if pix.Status != "ready" || err != nil || code.Amount != money.New(10150, money.BRL) {
		t.Fatalf("pix = %+v, parsed %+v, %v; want a ready code for R$ 101.50", pix, code, err)
	}
	id := paymentID(t, rec)
	stored := c.attemptOf(id)
	charge, err := c.sim.Get(t.Context(), stored.id.String())
	if stored.status != "accepted" || err != nil || charge.ID != stored.externalID || charge.TxID != code.TxID {
		t.Errorf("attempt %+v, charge %+v, %v; want accepted and named after the charge", stored, charge, err)
	}
	if got := c.do(getRequest(t, key, "/v1/payments/"+id), true); got.Body.String() != rec.Body.String() {
		t.Errorf("GET differs from POST:\n got %s\nwant %s", got.Body, rec.Body)
	}
	var list struct{ Data []json.RawMessage }
	listed := c.do(getRequest(t, key, "/v1/payments"), true)
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil || len(list.Data) != 1 ||
		string(list.Data[0]) != strings.TrimSuffix(rec.Body.String(), "\n") {
		t.Errorf("list = %s, want the created payment", listed.Body)
	}
}

func TestProviderRefusalIsAFailedPix(t *testing.T) {
	c := newContract(t)
	if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpCreate, Kind: pixsim.Reject, Count: 1}); err != nil {
		t.Fatal(err)
	}
	rec := c.do(postPayment(t, c.merchantKey(), "order-123", gameStore), true)
	if pix := pixOf(t, rec); rec.Code != http.StatusCreated || pix != (pixField{Status: "failed"}) {
		t.Fatalf("got %d %+v, want 201 with a failed pix", rec.Code, pix)
	}
	if a := c.attemptOf(paymentID(t, rec)); a.status != "rejected" {
		t.Errorf("attempt status = %s, want rejected", a.status)
	}
}

// Section 4.3: an error from the provider is never a failure. The payment is created,
// the attempt waits in unknown for the resolver, and the merchant sees processing.
func TestAmbiguousProviderLeavesTheAttemptUnknown(t *testing.T) {
	for _, tc := range []struct {
		fault         pixsim.FaultKind
		chargeCreated bool
	}{
		{pixsim.TimeoutAfter, true},
		{pixsim.FailBefore, false},
	} {
		t.Run(string(tc.fault), func(t *testing.T) {
			c := newContract(t)
			if err := c.sim.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpCreate, Kind: tc.fault, Count: 1}); err != nil {
				t.Fatal(err)
			}
			key := c.merchantKey()
			begin := time.Now()
			rec := c.do(postPayment(t, key, "order-123", gameStore), true)
			if elapsed := time.Since(begin); elapsed > pixsim.DefaultTimeout/2 {
				t.Errorf("create took %v, want it bounded by the %v provider timeout", elapsed, providerTimeout)
			}
			if pix := pixOf(t, rec); rec.Code != http.StatusCreated || pix != (pixField{Status: "processing"}) {
				t.Fatalf("got %d %+v, want 201 with a processing pix", rec.Code, pix)
			}
			a := c.attemptOf(paymentID(t, rec))
			if a.status != "unknown" {
				t.Errorf("attempt status = %s, want unknown", a.status)
			}
			_, err := c.sim.Get(t.Context(), a.id.String())
			if created := err == nil; created != tc.chargeCreated {
				t.Errorf("charge exists at the provider = %v (%v), want %v", created, err, tc.chargeCreated)
			}
		})
	}
}

// Retries are answered from the stored response; the provider is called once.
func TestRetriesCallTheProviderOnce(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	const workers = 10
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			c.handler.ServeHTTP(rec, postPayment(t, key, "order-123", gameStore))
		})
	}
	wg.Wait()
	final := c.do(postPayment(t, key, "order-123", gameStore), true)

	if pix := pixOf(t, final); pix.Status != "ready" {
		t.Errorf("final retry pix = %+v, want ready", pix)
	}
	if n := c.provider.creates.Load(); n != 1 || c.count("payment_attempts") != 1 {
		t.Errorf("provider called %d times for %d attempts, want 1 and 1", n, c.count("payment_attempts"))
	}
}

// crashedCreate is a request that held the key and committed its first transaction,
// the payment with its attempt in created, and then died.
func (c *contract) crashedCreate(merchantKey, idempotencyKey string) (*paymentsadapters.Postgres, paymentsapp.Priced) {
	c.t.Helper()
	ctx := c.t.Context()
	principal, err := merchantsapp.NewService(merchantsadapters.NewPostgres(c.pool), rand.Reader).Authenticate(ctx, merchantKey)
	if err != nil {
		c.t.Fatal(err)
	}
	keys := idempotencyapp.NewService(idempotencyadapters.NewPostgres(c.pool), c.clock, rand.Reader)
	req, err := idempotency.NewRequest(idempotency.MerchantID(principal.MerchantID), idempotencyKey,
		idempotency.HashRequest(http.MethodPost, "/v1/payments", []byte(gameStore)))
	if err != nil {
		c.t.Fatal(err)
	}
	acquired, err := keys.Begin(ctx, &req)
	if err != nil {
		c.t.Fatal(err)
	}
	rate, err := fx.ParseRate(money.USD, money.BRL, "5.0000")
	if err != nil {
		c.t.Fatal(err)
	}
	quote, err := fxapp.NewQuoter(fxadapters.NewFixedRates(rate), c.clock, fx.DefaultPricing).
		Quote(ctx, money.New(2000, money.USD), money.BRL, payments.PixQuoteValidity)
	if err != nil {
		c.t.Fatal(err)
	}
	draft, err := payments.NewDraft(payments.MerchantID(principal.MerchantID), "pix", money.New(2000, money.USD), "order-123")
	if err != nil {
		c.t.Fatal(err)
	}
	store := paymentsadapters.NewPostgres(c.pool, merchantOutbox{})
	p, err := store.Create(ctx, acquired.Held, &draft, &quote, c.provider.Name())
	if err != nil {
		c.t.Fatal(err)
	}
	return store, p
}

func TestResumeFromCreatedCallsTheProvider(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	_, p := c.crashedCreate(key, "order-123")

	c.clock.Advance(idempotency.Lease)
	rec := c.do(postPayment(t, key, "order-123", gameStore), true)
	if pix := pixOf(t, rec); rec.Code != http.StatusCreated || pix.Status != "ready" || paymentID(t, rec) != p.Payment.ID().String() {
		t.Fatalf("resumed create got %d %s, want 201 ready for %s", rec.Code, rec.Body, p.Payment.ID())
	}
	if n := c.provider.creates.Load(); n != 1 {
		t.Errorf("provider called %d times, want 1: created proves it was never called", n)
	}
}

// Decision 17: sent means the provider may have acted, so the resumed request never
// calls it again blind; the attempt waits in unknown for GetAttempt.
func TestResumeFromSentDoesNotCallAgain(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	store, p := c.crashedCreate(key, "order-123")
	if _, _, err := store.ApplyAttempt(t.Context(), p.Attempt.ID(), payments.AttemptUpdate{Event: payments.Send}, t0); err != nil {
		t.Fatal(err)
	}

	c.clock.Advance(idempotency.Lease)
	rec := c.do(postPayment(t, key, "order-123", gameStore), true)
	if pix := pixOf(t, rec); rec.Code != http.StatusCreated || pix.Status != "processing" {
		t.Fatalf("resumed create got %d %s, want 201 processing", rec.Code, rec.Body)
	}
	if n := c.provider.creates.Load(); n != 0 {
		t.Errorf("provider called %d times, want 0", n)
	}
	if a := c.attemptOf(p.Payment.ID().String()); a.status != "unknown" {
		t.Errorf("attempt status = %s, want unknown", a.status)
	}
}

// Section 4.3: a client that gives up does not undo what the provider did. The request
// is canceled while the provider is being called; the call and its answer still land.
func TestClientGivingUpDoesNotCutTheProviderCall(t *testing.T) {
	c := newContract(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.provider.before = cancel

	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, postPayment(t, c.merchantKey(), "order-123", gameStore).WithContext(ctx))
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("the request was not canceled during the call")
	}
	if pix := pixOf(t, rec); rec.Code != http.StatusCreated || pix.Status != "ready" {
		t.Fatalf("got %d %s, want 201 ready", rec.Code, rec.Body)
	}
	if a := c.attemptOf(paymentID(t, rec)); a.status != "accepted" {
		t.Errorf("attempt status = %s, want accepted", a.status)
	}
}
