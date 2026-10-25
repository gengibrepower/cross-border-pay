package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEveryRunsAtOnceAndThenRepeatedly(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var rounds atomic.Int64
	third := make(chan struct{})
	job := Every("count", time.Millisecond, discard, func(context.Context) error {
		if rounds.Add(1) == 3 {
			close(third)
		}
		return nil
	})

	result := runAsync(ctx, time.Second, job)
	select {
	case <-third:
	case <-time.After(5 * time.Second):
		t.Fatalf("%d rounds after 5 s, want 3", rounds.Load())
	}
	cancel()
	if err := await(t, result); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
}

func TestEveryWaitsForTheIntervalBetweenRounds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var rounds atomic.Int64
	result := runAsync(ctx, time.Second, Every("slow", time.Hour, discard, func(context.Context) error {
		rounds.Add(1)
		return nil
	}))
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := await(t, result); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if n := rounds.Load(); n != 1 {
		t.Errorf("%d rounds within the first interval, want just the one at start", n)
	}
}

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

// A failed round is logged and the job keeps going; it does not stop the worker the
// way a Job returning an error does.
func TestEveryLogsAFailedRoundAndKeepsGoing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var log syncBuffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	var rounds atomic.Int64
	recovered := make(chan struct{})
	job := Every("flaky", time.Millisecond, logger, func(context.Context) error {
		if rounds.Add(1) == 1 {
			return errors.New("database down")
		}
		if rounds.Load() == 2 {
			close(recovered)
		}
		return nil
	})

	result := runAsync(ctx, time.Second, job)
	select {
	case <-recovered:
	case err := <-result:
		t.Fatalf("Run() returned %v after a failed round, want the job to keep going", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no second round after the failed one")
	}
	cancel()
	if err := await(t, result); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if !strings.Contains(log.String(), "database down") {
		t.Errorf("log = %s, want the failed round", log.String())
	}
}
