package worker

import (
	"context"
	"log/slog"
	"time"
)

// Every logs a failed round and runs the next on schedule: the database briefly down
// must not stop the whole worker the way a failed Job does.
func Every(name string, interval time.Duration, logger *slog.Logger, fn func(ctx context.Context) error) Job {
	return Job{Name: name, Run: func(ctx context.Context) error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				logger.ErrorContext(ctx, "job round failed", "job", name, "error", err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}}
}
