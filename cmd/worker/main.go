package main

import (
	"context"
	"crypto/rand"
	"errors"
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
	merchantsadapters "github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/config"
	"github.com/gengibrepower/cross-border-pay/internal/platform/logging"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/platform/ssrf"
	"github.com/gengibrepower/cross-border-pay/internal/platform/worker"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
	webhooksadapters "github.com/gengibrepower/cross-border-pay/internal/webhooks/adapters"
	webhooksapp "github.com/gengibrepower/cross-border-pay/internal/webhooks/app"
)

const (
	idempotencyCleanupInterval = time.Hour
	// attemptResolverInterval is how often due attempts are looked for; each attempt's
	// own backoff lives in its next_check_at.
	attemptResolverInterval = time.Second
	// inboxInterval paces the inbox; a pending event's own backoff is in its row.
	inboxInterval = time.Second
	// expiryInterval is how often expired quotes are looked for; a payment the provider
	// could not be asked about comes back once its lease ends.
	expiryInterval = time.Second
	// webhookInterval paces the delivery rounds; each event's backoff is in its row.
	webhookInterval        = time.Second
	webhookCleanupInterval = time.Hour
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
		err = errors.Join(cfg.RequireDatabase(), cfg.RequireSigningKey())
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
	store, provider := paymentsadapters.NewPostgres(pool, paymentSinks()), paymentsadapters.NewPixSim(pixsimClient)
	resolver := paymentsapp.NewResolver(store, provider, clock.System{}, logger, cfg.ProviderTimeout)
	expirer := paymentsapp.NewExpirer(store, provider, clock.System{}, logger, cfg.ProviderTimeout)
	inbox := eventsapp.NewProcessor(eventsadapters.NewPostgres(pool), paymentEvents{clock: clock.System{}, sinks: paymentSinks()}, clock.System{}, logger)
	deliverer, err := webhooksapp.NewDeliverer(
		eventsadapters.NewDeliveries(pool), merchantsadapters.NewPostgres(pool),
		webhooksadapters.NewHTTPSender(ssrf.Policy{AllowPrivate: cfg.WebhookAllowPrivate}, cfg.WebhookTimeout),
		[]byte(cfg.WebhookSigningKey), clock.System{}, webhooksapp.FullJitter, cfg.WebhookTimeout, logger,
	)
	if err != nil {
		return fmt.Errorf("webhooks: %w", err)
	}

	logger.Info("worker started")
	if err := worker.Run(ctx, logger, cfg.ShutdownTimeout, jobs(pool, resolver, expirer, inbox, deliverer, logger)...); err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	logger.Info("worker stopped")
	return nil
}

func jobs(pool *pgxpool.Pool, resolver *paymentsapp.Resolver, expirer *paymentsapp.Expirer, inbox *eventsapp.Processor, deliverer *webhooksapp.Deliverer, logger *slog.Logger) []worker.Job {
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
		worker.Every("payment-expiry", expiryInterval, logger, func(ctx context.Context) error {
			if _, err := expirer.Expire(ctx, paymentsapp.ExpiryBatch); err != nil {
				return fmt.Errorf("expiry: %w", err)
			}
			return nil
		}),
		worker.Every("inbox", inboxInterval, logger, func(ctx context.Context) error {
			if _, err := inbox.Process(ctx, eventsapp.ProcessBatch); err != nil {
				return fmt.Errorf("inbox: %w", err)
			}
			return nil
		}),
		worker.Every("webhooks", webhookInterval, logger, func(ctx context.Context) error {
			if _, err := deliverer.Deliver(ctx, webhooksapp.DeliverBatch); err != nil {
				return fmt.Errorf("webhooks: %w", err)
			}
			return nil
		}),
		worker.Every("webhook-cleanup", webhookCleanupInterval, logger, func(ctx context.Context) error {
			n, err := deliverer.Cleanup(ctx, webhooksapp.CleanupBatch)
			if n > 0 {
				logger.InfoContext(ctx, "delivered webhook events deleted", "count", n)
			}
			if err != nil {
				return fmt.Errorf("cleanup: %w", err)
			}
			return nil
		}),
	}
}
