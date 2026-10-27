package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/config"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
	"github.com/gengibrepower/cross-border-pay/internal/platform/logging"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/platform/ssrf"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

const readinessTimeout = 2 * time.Second

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
		err = errors.Join(cfg.RequireDatabase(), cfg.RequireWebhookSecret(), cfg.RequireSigningKey())
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
	api, err := newAPI(pool, logger, &deps{
		usdbrl:          cfg.USDBRLRate,
		clock:           clock.System{},
		provider:        paymentsadapters.NewPixSim(pixsimClient),
		providerTimeout: cfg.ProviderTimeout,
		webhookSecret:   cfg.PixSimWebhookSecret,
		signingKey:      []byte(cfg.WebhookSigningKey),
		webhooks:        ssrf.Policy{AllowPrivate: cfg.WebhookAllowPrivate},
	})
	if err != nil {
		return err
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if err := httpapi.Serve(ctx, "api", ln, routes(pool, logger, api.register), logger, cfg.ShutdownTimeout); err != nil {
		return fmt.Errorf("http: %w", err)
	}
	return nil
}

type pinger interface {
	Ping(ctx context.Context) error
}

func routes(db pinger, logger *slog.Logger, register ...func(*http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyz(db))
	for _, r := range register {
		r(mux)
	}
	return httpapi.Handler(mux, logger)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeStatus(w, http.StatusOK, "ok")
}

func readyz(db pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			writeStatus(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		writeStatus(w, http.StatusOK, "ok")
	}
}

func writeStatus(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, `{"status":"`+status+`"}`)
}
