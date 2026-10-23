package worker

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

var discard = slog.New(slog.DiscardHandler)

func untilCanceled(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func runAsync(ctx context.Context, timeout time.Duration, jobs ...Job) <-chan error {
	result := make(chan error, 1)
	go func() { result <- Run(ctx, discard, timeout, jobs...) }()
	return result
}

func await(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func TestRunStopsJobsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan string, 2)
	job := func(name string) Job {
		return Job{Name: name, Run: func(ctx context.Context) error {
			err := untilCanceled(ctx)
			stopped <- name
			return err
		}}
	}

	result := runAsync(ctx, time.Second, job("a"), job("b"))
	cancel()

	if err := await(t, result); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if len(stopped) != 2 {
		t.Errorf("%d jobs stopped before Run returned, want 2", len(stopped))
	}
}

func TestRunWithoutJobsWaitsForCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	result := runAsync(ctx, time.Second)

	select {
	case err := <-result:
		t.Fatalf("Run() returned %v before cancel", err)
	case <-time.After(20 * time.Millisecond):
	}

	cancel()
	if err := await(t, result); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
}

func TestRunFailingJobStopsTheOthers(t *testing.T) {
	boom := errors.New("boom")
	failing := Job{Name: "failing", Run: func(context.Context) error { return boom }}
	waiting := Job{Name: "waiting", Run: untilCanceled}

	err := await(t, runAsync(t.Context(), time.Second, failing, waiting))

	if !errors.Is(err, boom) {
		t.Fatalf("Run() error = %v, want %v", err, boom)
	}
}

func TestRunGivesUpAfterShutdownTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	release := make(chan struct{})
	defer close(release)
	stuck := Job{Name: "stuck", Run: func(context.Context) error {
		<-release
		return nil
	}}

	result := runAsync(ctx, 10*time.Millisecond, stuck)
	cancel()

	if err := await(t, result); !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run() error = %v, want %v", err, ErrShutdownTimeout)
	}
}
