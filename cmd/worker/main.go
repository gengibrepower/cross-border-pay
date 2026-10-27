package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	eventsadapters "github.com/gengibrepower/cross-border-pay/internal/events/adapters"
	eventsapp "github.com/gengibrepower/cross-border-pay/internal/events/app"
	idempotencyadapters "github.com/gengibrepower/cross-border-pay/internal/idempotency/adapters"
	idempotencyapp "github.com/gengibrepower/cross-border-pay/internal/idempotency/app"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/config"
	"github.com/gengibrepower/cross-border-pay/internal/platform/logging"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/platform/worker"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

const (
	idempotencyCleanupInterval = time.Hour
	// attemptResolverInterval is how often due attempts are looked for; each attempt's
	// own backoff lives in its next_check_at.
	attemptResolverInterval = time.Second
	// inboxInterval paces the inbox; a pending event's own backoff is in its row.
	inboxInterval = time.Second
)

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

	pixsimClient, err := pixsim.NewClient(cfg.PixSimURL, &http.Client{Timeout: cfg.ProviderTimeout})
	if err != nil {
		return fmt.Errorf("config: PIXSIM_URL: %w", err)
	}
	resolver := paymentsapp.NewResolver(paymentsadapters.NewPostgres(pool), paymentsadapters.NewPixSim(pixsimClient),
		clock.System{}, logger, cfg.ProviderTimeout)
	inbox := eventsapp.NewProcessor(eventsadapters.NewPostgres(pool), paymentEvents{clock: clock.System{}}, clock.System{}, logger)

	logger.Info("worker started")
	if err := worker.Run(ctx, logger, cfg.ShutdownTimeout, jobs(pool, resolver, inbox, logger)...); err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	logger.Info("worker stopped")
	return nil
}

func jobs(pool *pgxpool.Pool, resolver *paymentsapp.Resolver, inbox *eventsapp.Processor, logger *slog.Logger) []worker.Job {
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
		worker.Every("attempt-resolver", attemptResolverInterval, logger, func(ctx context.Context) error {
			if _, err := resolver.Resolve(ctx, paymentsapp.ResolveBatch); err != nil {
				return fmt.Errorf("resolve: %w", err)
			}
			return nil
		}),
		worker.Every("inbox", inboxInterval, logger, func(ctx context.Context) error {
			if _, err := inbox.Process(ctx, eventsapp.ProcessBatch); err != nil {
				return fmt.Errorf("inbox: %w", err)
			}
			return nil
		}),
	}
}
