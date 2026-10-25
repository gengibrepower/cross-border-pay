package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	idempotencyadapters "github.com/gengibrepower/cross-border-pay/internal/idempotency/adapters"
	idempotencyapp "github.com/gengibrepower/cross-border-pay/internal/idempotency/app"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/config"
	"github.com/gengibrepower/cross-border-pay/internal/platform/logging"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/platform/worker"
)

const idempotencyCleanupInterval = time.Hour

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	cfg, err := config.Load(getenv)
	if err == nil {
		err = cfg.RequireDatabase()
	}
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	logger := logging.New(stdout, cfg.LogLevel)

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()

	logger.Info("worker started")
	if err := worker.Run(ctx, logger, cfg.ShutdownTimeout, jobs(pool, logger)...); err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	logger.Info("worker stopped")
	return nil
}

func jobs(pool *pgxpool.Pool, logger *slog.Logger) []worker.Job {
	keys := idempotencyapp.NewService(idempotencyadapters.NewPostgres(pool), clock.System{}, rand.Reader)
	return []worker.Job{
		worker.Every("idempotency-cleanup", idempotencyCleanupInterval, logger, func(ctx context.Context) error {
			n, err := keys.Cleanup(ctx, idempotencyapp.CleanupBatch)
			if n > 0 {
				logger.InfoContext(ctx, "expired idempotency keys deleted", "count", n)
			}
			if err != nil {
				return fmt.Errorf("cleanup: %w", err)
			}
			return nil
		}),
	}
}
