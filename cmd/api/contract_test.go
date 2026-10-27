package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5/pgxpool"

	merchantsadapters "github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	merchantsapp "github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	merchants "github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
	pixsimserver "github.com/gengibrepower/cross-border-pay/internal/providers/pixsim/server"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// contract serves requests through the real handler and checks each request and
// response against api/openapi.yaml (decision 50).
type contract struct {
	t       *testing.T
	doc     *openapi3.T
	router  routers.Router
	handler http.Handler
	pool    *pgxpool.Pool
	clock   *clock.Fake
	// sim is the PIX simulator's client, for injecting faults and paying charges.
	sim      *pixsim.Client
	provider *countingProvider
	deps     *deps
}

// countingProvider counts the calls to create an attempt and can run a hook first,
// so a test can act at the moment the provider is called.
type countingProvider struct {
	paymentsapp.Provider
	creates atomic.Int64
	before  func()
}

func (p *countingProvider) CreateAttempt(ctx context.Context, req *paymentsapp.AttemptRequest) (paymentsapp.AttemptResult, error) {
	p.creates.Add(1)
	if p.before != nil {
		p.before()
	}
	return p.Provider.CreateAttempt(ctx, req) //nolint:wrapcheck // a test double passes the provider's error through untouched.
}

// providerTimeout is short so that the timeout tests do not wait long.
const providerTimeout = 300 * time.Millisecond

// webhookSecret signs the simulator's events in these tests.
const webhookSecret = "test-webhook-secret" //nolint:gosec // G101: a signing key that exists only between this test and its in-process simulator.

// startPixSim starts the simulator sending its events to webhookURL.
func startPixSim(t *testing.T, c *clock.Fake, webhookURL string) *pixsim.Client {
	t.Helper()
	sim, err := pixsimserver.New(&pixsimserver.Config{
		Seed: 1, PixKey: "123e4567-e12b-12d1-a456-426655440000", MerchantName: "CROSS BORDER PAY", MerchantCity: "SAO PAULO",
		WebhookURL: webhookURL, WebhookSecret: []byte(webhookSecret),
	}, c, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	sim.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// The client waits longer than providerTimeout, so the timeout tests exercise the
	// payments service's own bound on the call.
	client, err := pixsim.NewClient(srv.URL, &http.Client{Timeout: pixsim.DefaultTimeout})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func newContract(t *testing.T) *contract {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(t.Context()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI document: %v", err)
	}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}

	pool := pgtest.NewPool(t)
	logger := slog.New(slog.DiscardHandler)
	fake := clock.NewFake(t0)
	// The simulator delivers its events over real HTTP to the API under test, which
	// is built after it: the hook forwards to it once it exists.
	c := &contract{t: t, doc: doc, router: router, pool: pool, clock: fake}
	var api atomic.Pointer[http.Handler]
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*api.Load()).ServeHTTP(w, r) }))
	t.Cleanup(hook.Close)
	c.sim = startPixSim(t, fake, hook.URL+"/v1/providers/pixsim/events")
	c.provider = &countingProvider{Provider: paymentsadapters.NewPixSim(c.sim)}
	c.deps = &deps{usdbrl: "5.0000", clock: fake, provider: c.provider, providerTimeout: providerTimeout, webhookSecret: webhookSecret}
	a, err := newAPI(pool, logger, c.deps)
	if err != nil {
		t.Fatal(err)
	}
	c.handler = routes(pool, logger, a.register)
	api.Store(&c.handler)
	return c
}

func (c *contract) merchantKey() string {
	c.t.Helper()
	svc := merchantsapp.NewService(merchantsadapters.NewPostgres(c.pool), rand.Reader)
	_, key, err := svc.CreateMerchant(c.t.Context(), "GameStore", merchants.Test)
	if err != nil {
		c.t.Fatal(err)
	}
	return key
}

// do sends the request; if validRequest, the request must also match the contract.
// The response must match it either way.
func (c *contract) do(req *http.Request, validRequest bool) *httptest.ResponseRecorder {
	c.t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))

	route, params, err := c.router.FindRoute(req)
	if err != nil {
		c.t.Fatalf("no route in openapi.yaml for %s %s: %v", req.Method, req.URL.Path, err)
	}
	input := &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: params,
		Route:      route,
		Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	if validRequest {
		if err := openapi3filter.ValidateRequest(c.t.Context(), input); err != nil {
			c.t.Fatalf("request does not match the contract: %v", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}

	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)

	err = openapi3filter.ValidateResponse(c.t.Context(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: input,
		Status:                 rec.Code,
		Header:                 rec.Header(),
		Body:                   io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	})
	if err != nil {
		c.t.Errorf("%d response does not match the contract: %v\n%s", rec.Code, err, rec.Body.String())
	}
	return rec
}

func postPayment(t *testing.T, key, idempotencyKey, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost:8080/v1/payments", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return req
}

const gameStore = `{"amount": {"value": 2000, "currency": "USD"}, "method": "pix", "reference": "order-123"}`

func TestCreatePaymentGameStore(t *testing.T) {
	c := newContract(t)
	rec := c.do(postPayment(t, c.merchantKey(), "k1", gameStore), true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Quote  struct {
			PayerAmount struct{ Value int64 } `json:"payer_amount"`
			BaseRate    string                `json:"base_rate"`
			Fee         struct{ Value int64 } `json:"fee"`
			NetAmount   struct{ Value int64 } `json:"net_amount"`
			ExpiresAt   time.Time             `json:"expires_at"`
		} `json:"quote"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// Section 3.3: US$ 20.00 → R$ 101.50, fee US$ 0.60, merchant gets US$ 19.40.
	if got.Status != "pending" || got.Quote.PayerAmount.Value != 10150 || got.Quote.Fee.Value != 60 ||
		got.Quote.NetAmount.Value != 1940 || got.Quote.BaseRate != "5.00000000" {
		t.Errorf("payment = %+v", got)
	}
	if !got.CreatedAt.Equal(t0) || !got.Quote.ExpiresAt.Equal(t0.Add(30*time.Minute)) {
		t.Errorf("created %s, expires %s; want %s and 30 minutes later", got.CreatedAt, got.Quote.ExpiresAt, t0)
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/payments/"+got.ID {
		t.Errorf("Location = %q, want /v1/payments/%s", loc, got.ID)
	}

	var payments, quotes int
	if err := c.pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM payments), (SELECT count(*) FROM quotes)").Scan(&payments, &quotes); err != nil {
		t.Fatal(err)
	}
	if payments != 1 || quotes != 1 {
		t.Errorf("%d payments and %d quotes stored, want 1 and 1", payments, quotes)
	}
}

func TestCreatePaymentErrors(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	tests := []struct {
		name         string
		key, idemKey string
		body         string
		validRequest bool
		wantStatus   int
		wantCode     string
	}{
		{"no API key", "", "k", gameStore, false, http.StatusUnauthorized, "unauthorized"},
		{"wrong API key", "sk_test_nope", "k", gameStore, false, http.StatusUnauthorized, "unauthorized"},
		{"no Idempotency-Key", key, "", gameStore, false, http.StatusBadRequest, "idempotency_key_required"},
		{"decimal value", key, "k", `{"amount": {"value": 20.5, "currency": "USD"}, "method": "pix", "reference": "o"}`, false, http.StatusBadRequest, "invalid_amount"},
		{"value as string", key, "k", `{"amount": {"value": "2000", "currency": "USD"}, "method": "pix", "reference": "o"}`, false, http.StatusBadRequest, "invalid_amount"},
		{"unknown currency", key, "k", `{"amount": {"value": 2000, "currency": "EUR"}, "method": "pix", "reference": "o"}`, false, http.StatusBadRequest, "invalid_currency"},
		{"unknown field", key, "k", `{"amount": {"value": 2000, "currency": "USD"}, "method": "pix", "reference": "o", "tip": 1}`, false, http.StatusBadRequest, "invalid_request"},
		{"no amount", key, "k", `{"method": "pix", "reference": "o"}`, false, http.StatusBadRequest, "invalid_request"},
		{"not JSON", key, "k", `amount=2000`, false, http.StatusBadRequest, "invalid_request"},
		{"unknown method", key, "k", `{"amount": {"value": 2000, "currency": "USD"}, "method": "crypto", "reference": "o"}`, false, http.StatusBadRequest, "invalid_method"},
		{"boleto not yet", key, "k", `{"amount": {"value": 2000, "currency": "USD"}, "method": "boleto", "reference": "o"}`, true, http.StatusUnprocessableEntity, "unsupported_method"},
		{"charged in BRL", key, "k", `{"amount": {"value": 2000, "currency": "BRL"}, "method": "pix", "reference": "o"}`, true, http.StatusUnprocessableEntity, "unsupported_currency"},
		{"zero amount", key, "k", `{"amount": {"value": 0, "currency": "USD"}, "method": "pix", "reference": "o"}`, true, http.StatusUnprocessableEntity, "invalid_amount"},
		{"empty reference", key, "k", `{"amount": {"value": 2000, "currency": "USD"}, "method": "pix", "reference": ""}`, false, http.StatusUnprocessableEntity, "invalid_reference"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.t = t
			// Each case gets its own key: the same key with another body is a 422 now.
			idemKey := tt.idemKey
			if idemKey == "k" {
				idemKey = tt.name
			}
			rec := c.do(postPayment(t, tt.key, idemKey, tt.body), tt.validRequest)
			var p struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tt.wantStatus || p.Code != tt.wantCode {
				t.Errorf("got %d %s, want %d %s", rec.Code, p.Code, tt.wantStatus, tt.wantCode)
			}
		})
	}

	var n int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM payments").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d payments stored by rejected requests", n)
	}
}
