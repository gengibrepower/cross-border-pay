package adapters_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func newService(t *testing.T) *app.Service {
	t.Helper()
	return app.NewService(adapters.NewPostgres(pgtest.NewPool(t)), rand.Reader)
}

func TestAuthenticate(t *testing.T) {
	svc := newService(t)
	ctx := t.Context()
	id, key, err := svc.CreateMerchant(ctx, "  GameStore  ", domain.Test)
	if err != nil {
		t.Fatal(err)
	}

	p, err := svc.Authenticate(ctx, key)
	if err != nil || p != (domain.Principal{MerchantID: id, Mode: domain.Test}) {
		t.Fatalf("Authenticate(own key) = %+v, %v; want merchant %s in test mode", p, err, id)
	}

	_, lookup, err := domain.ParseKey(key)
	if err != nil {
		t.Fatal(err)
	}
	other, err := domain.GenerateKey(domain.Test, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := other.Plaintext[strings.LastIndex(other.Plaintext, "_")+1:]
	rejected := map[string]string{
		"malformed":                  "sk_test_nope",
		"unknown lookup":             other.Plaintext,
		"right lookup, wrong secret": "sk_test_" + lookup + "_" + secret,
		"right key, other mode":      strings.Replace(key, "_test_", "_live_", 1),
	}
	for name, k := range rejected {
		if _, err := svc.Authenticate(ctx, k); !errors.Is(err, app.ErrUnauthorized) {
			t.Errorf("%s: Authenticate error = %v, want %v", name, err, app.ErrUnauthorized)
		}
	}

	if err := svc.RevokeKey(ctx, lookup); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, key); !errors.Is(err, app.ErrUnauthorized) {
		t.Errorf("revoked key: error = %v, want %v", err, app.ErrUnauthorized)
	}
	if err := svc.RevokeKey(ctx, lookup); !errors.Is(err, app.ErrKeyNotFound) {
		t.Errorf("revoking twice: error = %v, want %v", err, app.ErrKeyNotFound)
	}
}

func TestCreateMerchantRejectsBadNames(t *testing.T) {
	svc := newService(t)
	for _, name := range []string{"", "   ", strings.Repeat("a", 201)} {
		if _, _, err := svc.CreateMerchant(t.Context(), name, domain.Test); !errors.Is(err, app.ErrInvalidName) {
			t.Errorf("CreateMerchant(%d chars) error = %v, want %v", len(name), err, app.ErrInvalidName)
		}
	}
	if _, _, err := svc.CreateMerchant(t.Context(), strings.Repeat("é", 200), domain.Test); err != nil {
		t.Errorf("200 two-byte characters: error = %v", err)
	}
}

type fakeAuth struct {
	principal domain.Principal
	err       error
}

func (f fakeAuth) Authenticate(context.Context, string) (domain.Principal, error) {
	return f.principal, f.err
}

func TestRequireAPIKey(t *testing.T) {
	merchant := domain.Principal{MerchantID: domain.MerchantID{1}, Mode: domain.Test}
	tests := []struct {
		name       string
		auth       fakeAuth
		header     string
		wantStatus int
	}{
		{"valid key", fakeAuth{principal: merchant}, "Bearer sk_test_x", http.StatusOK},
		{"no header", fakeAuth{principal: merchant}, "", http.StatusUnauthorized},
		{"basic auth", fakeAuth{principal: merchant}, "Basic c2s6", http.StatusUnauthorized},
		{"rejected key", fakeAuth{err: app.ErrUnauthorized}, "Bearer sk_test_x", http.StatusUnauthorized},
		{"store down", fakeAuth{err: errors.New("connection refused")}, "Bearer sk_test_x", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle("GET /v1/whoami", adapters.RequireAPIKey(tt.auth, slog.New(slog.DiscardHandler))(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					p, ok := adapters.PrincipalFrom(r.Context())
					if !ok || p != merchant {
						t.Errorf("PrincipalFrom = %+v, %v; want %+v", p, ok, merchant)
					}
					w.WriteHeader(http.StatusOK)
				})))

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/whoami", http.NoBody)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			httpapi.Handler(mux, slog.New(slog.DiscardHandler)).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusOK {
				return
			}
			var p httpapi.Problem
			if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			if tt.wantStatus == http.StatusUnauthorized {
				if p.Code != "unauthorized" || rec.Header().Get("WWW-Authenticate") == "" {
					t.Errorf("problem code %q, WWW-Authenticate %q", p.Code, rec.Header().Get("WWW-Authenticate"))
				}
			}
			if strings.Contains(rec.Body.String(), "connection refused") {
				t.Errorf("response leaks the store error: %s", rec.Body.String())
			}
		})
	}
}
