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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

func TestUpdateEndpointOfAnUnknownMerchant(t *testing.T) {
	store := adapters.NewPostgres(pgtest.NewPool(t))
	_, err := store.UpdateEndpoint(t.Context(), domain.MerchantID{9}, func(*domain.Endpoint) (domain.Endpoint, error) {
		t.Error("changed the endpoint of a merchant that does not exist")
		return domain.Endpoint{}, nil
	})
	if !errors.Is(err, app.ErrMerchantNotFound) {
		t.Errorf("error = %v, want %v", err, app.ErrMerchantNotFound)
	}
}

// The lost update the merchant's lock prevents, forced rather than left to chance: a URL
// change has read version 1 when a rotation starts. The rotation must wait for it and
// then rotate what it wrote; without the lock it would commit first, and the URL
// change would write version 1 back over it.
func TestAURLChangeCannotUndoARotation(t *testing.T) {
	pool := pgtest.NewPool(t)
	store := adapters.NewPostgres(pool)
	merchant, _, err := app.NewService(store, rand.Reader).CreateMerchant(t.Context(), "GameStore", domain.Test)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	put := func(c domain.Change) func(*domain.Endpoint) (domain.Endpoint, error) {
		return func(current *domain.Endpoint) (domain.Endpoint, error) {
			e, _, err := domain.Apply(current, merchant, c, t0)
			return e, err
		}
	}
	if _, err := store.UpdateEndpoint(t.Context(), merchant, put(domain.Change{URL: "https://a.example"})); err != nil {
		t.Fatal(err)
	}

	rotated := make(chan error, 1)
	started := false
	_, err = store.UpdateEndpoint(t.Context(), merchant, func(current *domain.Endpoint) (domain.Endpoint, error) {
		if !started {
			started = true
			go func() {
				_, err := store.UpdateEndpoint(context.WithoutCancel(t.Context()), merchant, put(domain.Change{URL: "https://a.example", RotateFrom: 1}))
				rotated <- err
			}()
			waitBlockedOrDone(t, pool, rotated)
		}
		return put(domain.Change{URL: "https://b.example"})(current)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-rotated; err != nil {
		t.Fatal(err)
	}
	var url string
	var version int
	if err := pool.QueryRow(t.Context(), "SELECT url, secret_version FROM webhook_endpoints").Scan(&url, &version); err != nil {
		t.Fatal(err)
	}
	// The rotation, a PUT of the whole endpoint, commits last: its URL stands.
	if url != "https://a.example" || version != 2 {
		t.Errorf("endpoint %s at version %d, want the rotation's at version 2", url, version)
	}
}

// waitBlockedOrDone returns once another backend waits on a lock, or done has a value,
// which it puts back.
func waitBlockedOrDone(t *testing.T, pool *pgxpool.Pool, done chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			done <- err
			return
		default:
		}
		var waiting bool
		err := pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock')").Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the rotation neither finished nor waited")
}
