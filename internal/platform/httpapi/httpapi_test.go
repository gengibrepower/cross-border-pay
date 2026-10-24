package httpapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

func serve(t *testing.T, logs *bytes.Buffer, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "missing" {
			httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, "thing_not_found", "no thing missing"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /panic", func(http.ResponseWriter, *http.Request) {
		panic("secret internal state")
	})
	mux.HandleFunc("GET /abort", func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})

	handler := httpapi.Handler(mux, slog.New(slog.NewJSONHandler(logs, nil)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody))
	return rec
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) httpapi.Problem {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
	var p httpapi.Problem
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProblemResponses(t *testing.T) {
	tests := []struct {
		name, method, path string
		wantStatus         int
		wantCode           string
	}{
		{"handler problem", http.MethodGet, "/things/missing", http.StatusNotFound, "thing_not_found"},
		{"no route", http.MethodGet, "/nothing", http.StatusNotFound, "not_found"},
		{"wrong method", http.MethodDelete, "/things/1", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"panic", http.MethodGet, "/panic", http.StatusInternalServerError, "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, &bytes.Buffer{}, tt.method, tt.path)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			p := decodeProblem(t, rec)
			if p.Status != tt.wantStatus || p.Code != tt.wantCode || p.Type != "/problems/"+tt.wantCode {
				t.Errorf("problem = %+v, want status %d and code %s", p, tt.wantStatus, tt.wantCode)
			}
			if p.Title != http.StatusText(tt.wantStatus) || p.Instance != tt.path {
				t.Errorf("problem = %+v, want title %q and instance %q", p, http.StatusText(tt.wantStatus), tt.path)
			}
			if p.RequestID == "" || p.RequestID != rec.Header().Get("X-Request-Id") {
				t.Errorf("request_id = %q, header = %q; want the same, non-empty", p.RequestID, rec.Header().Get("X-Request-Id"))
			}
		})
	}
}

func TestWrongMethodKeepsAllow(t *testing.T) {
	rec := serve(t, &bytes.Buffer{}, http.MethodDelete, "/things/1")
	if got := rec.Header().Get("Allow"); !strings.Contains(got, http.MethodGet) {
		t.Errorf("Allow = %q, want it to list GET", got)
	}
}

func TestPanicIsLoggedButNotLeaked(t *testing.T) {
	var logs bytes.Buffer
	rec := serve(t, &logs, http.MethodGet, "/panic")

	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("response leaks the panic: %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "secret internal state") {
		t.Errorf("panic not logged: %s", logs.String())
	}
	if !strings.Contains(logs.String(), rec.Header().Get("X-Request-Id")) {
		t.Errorf("log lacks the request ID %q: %s", rec.Header().Get("X-Request-Id"), logs.String())
	}
}

func TestAbortHandlerStillAborts(t *testing.T) {
	defer func() {
		v := recover()
		if err, ok := v.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("recover() = %v, want http.ErrAbortHandler", v)
		}
	}()
	serve(t, &bytes.Buffer{}, http.MethodGet, "/abort")
}

func TestRequestIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := serve(t, &bytes.Buffer{}, http.MethodGet, "/things/1").Header().Get("X-Request-Id")
		if id == "" || seen[id] {
			t.Fatalf("request ID %q is empty or repeated", id)
		}
		seen[id] = true
	}
}

func TestAccessLog(t *testing.T) {
	var logs bytes.Buffer
	rec := serve(t, &logs, http.MethodGet, "/things/1")

	var entry struct {
		Msg       string `json:"msg"`
		Status    int    `json:"status"`
		Method    string `json:"method"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log line %q: %v", logs.String(), err)
	}
	if entry.Msg != "request" || entry.Status != http.StatusNoContent || entry.Method != http.MethodGet ||
		entry.RequestID != rec.Header().Get("X-Request-Id") {
		t.Errorf("access log = %+v", entry)
	}
}
