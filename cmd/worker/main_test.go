package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/platform/config"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

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

// The worker starts its jobs, the cleanup and the attempt resolver, against a real database and stops cleanly on cancel,
// as on SIGTERM.
func TestRunStartsJobsAndStopsWhenCanceled(t *testing.T) {
	url := pgtest.NewDatabase(t)
	getenv := func(key string) string {
		if key == "DATABASE_URL" {
			return url
		}
		return ""
	}
	ctx, cancel := context.WithCancel(t.Context())
	var out syncBuffer
	result := make(chan error, 1)
	go func() { result <- run(ctx, getenv, &out) }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), `"job":"idempotency-cleanup"`) || !strings.Contains(out.String(), `"job":"attempt-resolver"`) {
		if time.Now().After(deadline) {
			t.Fatalf("jobs not started after 5 s; log:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
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
	if !strings.Contains(out.String(), "worker stopped") {
		t.Errorf("log = %s, want a \"worker stopped\" entry", out.String())
	}
}

func TestRunRequiresDatabase(t *testing.T) {
	err := run(t.Context(), func(string) string { return "" }, io.Discard)
	if !errors.Is(err, config.ErrMissingDatabaseURL) {
		t.Fatalf("run() error = %v, want %v", err, config.ErrMissingDatabaseURL)
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	getenv := func(key string) string {
		if key == "SHUTDOWN_TIMEOUT" {
			return "ten"
		}
		return ""
	}
	if err := run(t.Context(), getenv, io.Discard); err == nil {
		t.Fatal("run() error = nil, want config error")
	}
}
