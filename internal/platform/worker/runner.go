package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

var ErrShutdownTimeout = errors.New("worker: jobs did not stop before the shutdown timeout")

type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

// Run blocks until ctx is canceled or a job fails, even when every job has
// already returned, so the process stays up for the supervisor to stop it.
func Run(ctx context.Context, logger *slog.Logger, shutdownTimeout time.Duration, jobs ...Job) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Go(func() {
			logger.Info("job started", "job", job.Name)
			err := job.Run(ctx)
			if err != nil && !(errors.Is(err, context.Canceled) && ctx.Err() != nil) {
				errs[i] = fmt.Errorf("job %s: %w", job.Name, err)
				logger.Error("job failed", "job", job.Name, "error", err)
				cancel()
			}
			logger.Info("job stopped", "job", job.Name)
		})
	}

	<-ctx.Done()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return errors.Join(errs...)
	case <-time.After(shutdownTimeout):
		return ErrShutdownTimeout
	}
}
