package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestRunServesAndStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var out syncBuffer
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, env(map[string]string{"PIXSIM_ADDR": "127.0.0.1:0", "PIXSIM_WEBHOOK_SECRET": "test-secret"}), &out)
	}()

	var addr string
	deadline := time.Now().Add(5 * time.Second)
	for addr == "" {
		for line := range strings.Lines(out.String()) {
			var entry struct{ Msg, Addr string }
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "pixsim started" {
				addr = entry.Addr
			}
		}
		if addr == "" && time.Now().After(deadline) {
			t.Fatalf("pixsim not started after 5 s; log:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/healthz", http.NoBody)
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
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not return after cancel")
	}
}

func TestLoad(t *testing.T) {
	secret := map[string]string{"PIXSIM_WEBHOOK_SECRET": "s3cret"}
	s, err := load(env(secret))
	if err != nil || s.addr != defaultAddr || s.sim.Seed != defaultSeed || s.sim.PixKey != defaultPixKey ||
		s.sim.WebhookURL != defaultWebhookURL || string(s.sim.WebhookSecret) != "s3cret" {
		t.Errorf("load(defaults) = %+v, %v", s, err)
	}
	s, err = load(env(map[string]string{
		"PIXSIM_ADDR": ":9000", "PIXSIM_SEED": "18446744073709551615", "PIXSIM_PIX_KEY": "k@example.com",
		"PIXSIM_WEBHOOK_URL": "http://api:8080/hook", "PIXSIM_WEBHOOK_SECRET": "s3cret",
	}))
	if err != nil || s.addr != ":9000" || s.sim.Seed != 1<<64-1 || s.sim.PixKey != "k@example.com" || s.sim.WebhookURL != "http://api:8080/hook" {
		t.Errorf("load(set) = %+v, %v", s, err)
	}
	for name, vars := range map[string]map[string]string{
		"bad addr":      {"PIXSIM_ADDR": "9000", "PIXSIM_WEBHOOK_SECRET": "s3cret"},
		"negative seed": {"PIXSIM_SEED": "-1", "PIXSIM_WEBHOOK_SECRET": "s3cret"},
		"text seed":     {"PIXSIM_SEED": "abc", "PIXSIM_WEBHOOK_SECRET": "s3cret"},
		"no secret":     {},
	} {
		if _, err := load(env(vars)); err == nil {
			t.Errorf("%s: load() succeeded", name)
		}
	}
}

func TestRunRejectsAnInvalidReceiver(t *testing.T) {
	err := run(t.Context(), env(map[string]string{"PIXSIM_PIX_KEY": strings.Repeat("k", 78), "PIXSIM_WEBHOOK_SECRET": "s3cret"}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "config") {
		t.Errorf("run() error = %v, want a config error", err)
	}
}
