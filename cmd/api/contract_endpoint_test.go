package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	merchants "github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
)

func putEndpoint(t *testing.T, key, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "http://localhost:8080/v1/webhook-endpoint", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

type endpoint struct {
	URL           string `json:"url"`
	SecretVersion int    `json:"secret_version"`
	Secret        string `json:"secret"`
}

func (c *contract) putEndpoint(key, body string, status int) endpoint {
	c.t.Helper()
	rec := c.do(putEndpoint(c.t, key, body), status != http.StatusBadRequest)
	if rec.Code != status {
		c.t.Fatalf("PUT %s: status %d, want %d: %s", body, rec.Code, status, rec.Body)
	}
	var e endpoint
	if status == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			c.t.Fatal(err)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			c.t.Error("an answer that can carry a secret is cacheable")
		}
	}
	return e
}

// merchantOf is the merchant whose endpoint is at url.
func (c *contract) merchantOf(url string) merchants.MerchantID {
	c.t.Helper()
	var id merchants.MerchantID
	if err := c.pool.QueryRow(c.t.Context(), "SELECT merchant_id FROM webhook_endpoints WHERE url = $1", url).Scan(&id); err != nil {
		c.t.Fatal(err)
	}
	return id
}

func (c *contract) secret(merchant merchants.MerchantID, v int) string {
	c.t.Helper()
	s, err := merchants.DeriveSecret([]byte(signingKey), merchant, merchants.SecretVersion(v))
	if err != nil {
		c.t.Fatal(err)
	}
	return s
}

func TestWebhookEndpointLifecycle(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()

	first := c.putEndpoint(key, `{"url":"https://gamestore.example/hooks"}`, http.StatusOK)
	merchant := c.merchantOf("https://gamestore.example/hooks")
	if first.SecretVersion != 1 || first.Secret != c.secret(merchant, 1) {
		t.Fatalf("first registration %+v, want version 1 with its secret", first)
	}

	moved := c.putEndpoint(key, `{"url":"https://gamestore.example/v2/hooks"}`, http.StatusOK)
	if moved.URL != "https://gamestore.example/v2/hooks" || moved.SecretVersion != 1 || moved.Secret != "" {
		t.Errorf("new URL %+v, want version 1 and no secret", moved)
	}

	rotated := c.putEndpoint(key, `{"url":"https://gamestore.example/v2/hooks","rotate_secret_from":1}`, http.StatusOK)
	if rotated.SecretVersion != 2 || rotated.Secret != c.secret(merchant, 2) || rotated.Secret == first.Secret {
		t.Errorf("rotation %+v, want version 2 with a new secret", rotated)
	}
	again := c.putEndpoint(key, `{"url":"https://gamestore.example/v2/hooks","rotate_secret_from":1}`, http.StatusOK)
	if again != rotated {
		t.Errorf("the same rotation again %+v, want %+v", again, rotated)
	}
	c.putEndpoint(key, `{"url":"https://gamestore.example/v2/hooks","rotate_secret_from":5}`, http.StatusConflict)

	var row string
	if err := c.pool.QueryRow(t.Context(), "SELECT row_to_json(w)::text FROM webhook_endpoints AS w").Scan(&row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, first.Secret) || strings.Contains(row, rotated.Secret) || strings.Contains(row, "whsec_") {
		t.Errorf("a secret is stored: %s", row)
	}
}

func TestWebhookEndpointRefusesInternalURLs(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	for _, url := range []string{
		"http://gamestore.example/hooks",
		"https://169.254.169.254/latest/meta-data/",
		"https://10.0.0.5/hooks",
		"https://[::1]/hooks",
		"https://localhost/hooks",
		"https://metadata.google.internal/",
		"https://2130706433/",
		"https://user:pass@gamestore.example/",
	} {
		c.putEndpoint(key, `{"url":"`+url+`"}`, http.StatusUnprocessableEntity)
	}
	c.putEndpoint(key, `{}`, http.StatusBadRequest)
	c.putEndpoint(key, `{"url":"https://gamestore.example","secret":"whsec_mine"}`, http.StatusBadRequest)
	c.putEndpoint(key, `{"url":"https://gamestore.example","rotate_secret_from":0}`, http.StatusBadRequest)
	c.putEndpoint("", `{"url":"https://gamestore.example"}`, http.StatusUnauthorized)
	c.putEndpoint(key, `{"url":"https://gamestore.example","rotate_secret_from":1}`, http.StatusConflict)

	var n int
	if err := c.pool.QueryRow(t.Context(), "SELECT count(*) FROM webhook_endpoints").Scan(&n); err != nil || n != 0 {
		t.Errorf("%d endpoints stored (%v), want none", n, err)
	}
}

func TestEachMerchantHasItsOwnEndpoint(t *testing.T) {
	c := newContract(t)
	a, b := c.merchantKey(), c.merchantKey()
	ea := c.putEndpoint(a, `{"url":"https://a.example/hooks"}`, http.StatusOK)
	eb := c.putEndpoint(b, `{"url":"https://b.example/hooks"}`, http.StatusOK)
	c.putEndpoint(b, `{"url":"https://b.example/hooks","rotate_secret_from":1}`, http.StatusOK)
	if ea.Secret == eb.Secret {
		t.Error("two merchants got the same secret")
	}
	var url string
	var version int
	err := c.pool.QueryRow(t.Context(), "SELECT url, secret_version FROM webhook_endpoints WHERE merchant_id = $1",
		c.merchantOf("https://a.example/hooks")).Scan(&url, &version)
	if err != nil || url != "https://a.example/hooks" || version != 1 {
		t.Errorf("merchant A's endpoint %s v%d (%v) after B's changes", url, version, err)
	}
}

// Concurrent rotations from the same version, as a client retrying in parallel: the
// merchant row's lock makes them one rotation, and every answer shows its secret.
func TestConcurrentRotationsRotateOnce(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	c.putEndpoint(key, `{"url":"https://gamestore.example/hooks"}`, http.StatusOK)

	const callers = 20
	got := make([]endpoint, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			c.handler.ServeHTTP(rec, putEndpoint(t, key, `{"url":"https://gamestore.example/hooks","rotate_secret_from":1}`))
			if rec.Code != http.StatusOK {
				t.Errorf("caller %d: %d %s", i, rec.Code, rec.Body)
				return
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got[i]); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	want := endpoint{URL: "https://gamestore.example/hooks", SecretVersion: 2, Secret: c.secret(c.merchantOf("https://gamestore.example/hooks"), 2)}
	for i, e := range got {
		if e != want {
			t.Errorf("caller %d got %+v, want %+v", i, e, want)
		}
	}
}

// The secret leaves the API only in the response body: the access log has none.
func TestWebhookSecretIsNotLogged(t *testing.T) {
	c := newContract(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a, err := newAPI(c.pool, logger, c.deps)
	if err != nil {
		t.Fatal(err)
	}
	h := routes(c.pool, logger, a.register)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, putEndpoint(t, c.merchantKey(), `{"url":"https://gamestore.example/hooks"}`))
	var e endpoint
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Secret == "" {
		t.Fatalf("no secret in %s (%v)", rec.Body, err)
	}
	if logs.Len() == 0 || strings.Contains(logs.String(), e.Secret) || strings.Contains(logs.String(), "whsec_") {
		t.Errorf("logs hold the secret, or nothing was logged:\n%s", logs.String())
	}
}
