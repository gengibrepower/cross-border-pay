// Command cardsim runs the card simulator: the acquirer the gateway talks to in test
// mode, with authorizations in memory, a seed and failures injected through /control.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/config"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
	"github.com/gengibrepower/cross-border-pay/internal/platform/logging"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim/server"
)

const (
	defaultAddr = ":8091"
	defaultSeed = 1
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

type settings struct {
	addr string
	sim  server.Config
}

func load(getenv func(string) string) (settings, error) {
	s := settings{addr: defaultAddr, sim: server.Config{Seed: defaultSeed}}
	var errs []error
	if v := getenv("CARDSIM_ADDR"); v != "" {
		if _, _, err := net.SplitHostPort(v); err != nil {
			errs = append(errs, fmt.Errorf("CARDSIM_ADDR: %w", err))
		}
		s.addr = v
	}
	if v := getenv("CARDSIM_SEED"); v != "" {
		seed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("CARDSIM_SEED: %w", err))
		}
		s.sim.Seed = seed
	}
	return s, errors.Join(errs...)
}

func run(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	s, err := load(getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	logger := logging.New(stdout, cfg.LogLevel)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	server.New(&s.sim, clock.System{}, logger).Register(mux)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	logger.Info("card simulator configured", "seed", s.sim.Seed)
	if err := httpapi.Serve(ctx, "cardsim", ln, httpapi.Handler(mux, logger), logger, cfg.ShutdownTimeout); err != nil {
		return fmt.Errorf("http: %w", err)
	}
	return nil
}
