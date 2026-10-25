package adapters_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/idempotency/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

var discard = slog.New(slog.DiscardHandler)

// handler counts its runs and records the key each run held.
type handler struct {
	runs  int
	held  []domain.Held
	serve func(w http.ResponseWriter, r *http.Request, held *domain.Held)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.runs++
	held, ok := adapters.HeldFrom(r.Context())
	if !ok {
		panic("no held key in the context")
	}
	h.held = append(h.held, *held)
	h.serve(w, r, held)
}

func (e *env) route(h *handler) http.Handler {
	m := e.merchant()
	merchant := func(context.Context) (domain.MerchantID, bool) { return m, true }
	return adapters.Require(e.svc, merchant, discard)(h)
}

func post(t *testing.T, srv http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/payments", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func respond(status int, body string) func(http.ResponseWriter, *http.Request, *domain.Held) {
	return func(w http.ResponseWriter, _ *http.Request, _ *domain.Held) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/v1/payments/pay_1")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestRequireReplaysAFinishedRequestWithoutRunningItAgain(t *testing.T) {
	e := newEnv(t)
	h := &handler{serve: respond(http.StatusCreated, `{"id":"pay_1"}`)}
	srv := e.route(h)

	first := post(t, srv, "k", "body")
	retry := post(t, srv, "k", "body")
	if h.runs != 1 {
		t.Fatalf("handler ran %d times, want 1", h.runs)
	}
	if retry.Code != first.Code || retry.Body.String() != first.Body.String() ||
		retry.Header().Get("Location") != first.Header().Get("Location") ||
		retry.Header().Get("Content-Type") != first.Header().Get("Content-Type") {
		t.Errorf("retry got %d %v %s, want the first answer %d %v %s",
			retry.Code, retry.Header(), retry.Body, first.Code, first.Header(), first.Body)
	}
	if first.Header().Get("Idempotent-Replayed") != "" || retry.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("Idempotent-Replayed = %q then %q, want none then true",
			first.Header().Get("Idempotent-Replayed"), retry.Header().Get("Idempotent-Replayed"))
	}
}

func TestRequireHandsTheBodyToTheHandler(t *testing.T) {
	e := newEnv(t)
	var got string
	h := &handler{serve: func(w http.ResponseWriter, r *http.Request, _ *domain.Held) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusCreated)
	}}
	post(t, e.route(h), "k", `{"amount": 1}`)
	if got != `{"amount": 1}` {
		t.Errorf("handler read %q, want the body the middleware hashed", got)
	}
}

// Decision 16: nothing was done, so the key is released and the retry runs from scratch.
func TestRequireReleasesTheKeyOnAServerErrorBeforeAnyWork(t *testing.T) {
	e := newEnv(t)
	h := &handler{serve: respond(http.StatusServiceUnavailable, `{}`)}
	srv := e.route(h)

	if rec := post(t, srv, "k", "body"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the handler's 503", rec.Code)
	}
	h.serve = respond(http.StatusCreated, `{"id":"pay_1"}`)
	if rec := post(t, srv, "k", "body"); rec.Code != http.StatusCreated || h.runs != 2 {
		t.Errorf("retry got %d after %d runs, want 201 from a second run", rec.Code, h.runs)
	}
	if h.held[1].RecoveryPoint != domain.Started {
		t.Errorf("retry held %+v, want a fresh key", h.held[1])
	}
}

// Decision 16's exception: work was recorded, so the key is kept and the retry, at
// once and not 30 s later, resumes from the recovery point.
func TestRequireKeepsTheKeyOnAServerErrorAfterWork(t *testing.T) {
	e := newEnv(t)
	h := &handler{serve: func(w http.ResponseWriter, r *http.Request, held *domain.Held) {
		if err := adapters.SetRecoveryPoint(r.Context(), e.pool, held, "attempt_requested", [16]byte{3}); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}}
	srv := e.route(h)

	post(t, srv, "k", "body")
	h.serve = respond(http.StatusCreated, `{"id":"pay_1"}`)
	if rec := post(t, srv, "k", "body"); rec.Code != http.StatusCreated {
		t.Fatalf("retry got %d, want 201 from the resumed run", rec.Code)
	}
	if got := h.held[1]; got.RecoveryPoint != "attempt_requested" || got.ResourceID != [16]byte{3} {
		t.Errorf("retry held %+v, want it resumed at attempt_requested", got)
	}
}

// Decision 15: while one request holds the key, the same request again is an
// immediate 409, and the handler does not run for it.
func TestRequireAnswersConflictWhileTheKeyIsHeld(t *testing.T) {
	e := newEnv(t)
	var srv http.Handler
	var retry *httptest.ResponseRecorder
	h := &handler{serve: func(w http.ResponseWriter, _ *http.Request, _ *domain.Held) {
		retry = post(t, srv, "k", "body")
		w.WriteHeader(http.StatusCreated)
	}}
	srv = e.route(h)

	post(t, srv, "k", "body")
	if retry.Code != http.StatusConflict || problemCode(t, retry) != "idempotency_key_in_progress" || h.runs != 1 {
		t.Errorf("retry during the first request got %d %s after %d runs, want 409 idempotency_key_in_progress and 1 run", retry.Code, retry.Body, h.runs)
	}
}

func TestRequireRejects(t *testing.T) {
	e := newEnv(t)
	srv := e.route(&handler{serve: respond(http.StatusCreated, `{}`)})
	post(t, srv, "used", "body")
	tests := []struct {
		name, key, body string
		status          int
		code            string
	}{
		{"no key", "", "body", http.StatusBadRequest, "idempotency_key_required"},
		{"key too long", strings.Repeat("k", 256), "body", http.StatusBadRequest, "idempotency_key_required"},
		{"same key, other body", "used", "other body", http.StatusUnprocessableEntity, "idempotency_key_reused"},
		{"body over 1 MiB", "big", strings.Repeat("x", 1<<20+1), http.StatusRequestEntityTooLarge, "body_too_large"},
	}
	for _, tt := range tests {
		if rec := post(t, srv, tt.key, tt.body); rec.Code != tt.status || problemCode(t, rec) != tt.code {
			t.Errorf("%s: got %d %s, want %d %s", tt.name, rec.Code, rec.Body, tt.status, tt.code)
		}
	}
}

func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p httpapi.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p.Code
}
