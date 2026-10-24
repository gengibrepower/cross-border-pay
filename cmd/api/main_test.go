package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func get(t *testing.T, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody))
	return rec
}

func assertResponse(t *testing.T, rec *httptest.ResponseRecorder, wantCode int, wantBody string) {
	t.Helper()
	if rec.Code != wantCode {
		t.Errorf("status = %d, want %d", rec.Code, wantCode)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body = %s, want %s", got, wantBody)
	}
}

func TestHealthz(t *testing.T) {
	rec := get(t, routes(fakeDB{}, slog.New(slog.DiscardHandler)), http.MethodGet, "/healthz")
	assertResponse(t, rec, http.StatusOK, `{"status":"ok"}`)
}

func TestHealthzIgnoresDatabase(t *testing.T) {
	rec := get(t, routes(fakeDB{err: errors.New("down")}, slog.New(slog.DiscardHandler)), http.MethodGet, "/healthz")
	assertResponse(t, rec, http.StatusOK, `{"status":"ok"}`)
}

func TestHealthzRejectsOtherMethods(t *testing.T) {
	rec := get(t, routes(fakeDB{}, slog.New(slog.DiscardHandler)), http.MethodPost, "/healthz")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
}

func TestReadyz(t *testing.T) {
	rec := get(t, routes(fakeDB{}, slog.New(slog.DiscardHandler)), http.MethodGet, "/readyz")
	assertResponse(t, rec, http.StatusOK, `{"status":"ok"}`)
}

func TestReadyzWithDatabaseDown(t *testing.T) {
	rec := get(t, routes(fakeDB{err: errors.New("down")}, slog.New(slog.DiscardHandler)), http.MethodGet, "/readyz")
	assertResponse(t, rec, http.StatusServiceUnavailable, `{"status":"unavailable"}`)
}

func TestServeAnswersAndStopsOnCancel(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- serve(ctx, ln, routes(fakeDB{}, slog.New(slog.DiscardHandler)), slog.New(slog.DiscardHandler), time.Second)
	}()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/healthz", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("serve() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	getenv := func(key string) string {
		if key == "LOG_LEVEL" {
			return "loud"
		}
		return ""
	}
	if err := run(t.Context(), getenv, io.Discard); err == nil {
		t.Fatal("run() error = nil, want config error")
	}
}

func TestRunRequiresDatabaseURL(t *testing.T) {
	err := run(t.Context(), func(string) string { return "" }, io.Discard)
	if err == nil {
		t.Fatal("run() error = nil, want missing DATABASE_URL")
	}
}
