// Command boletosim runs the boleto simulator: the bank the gateway talks to in test
// mode, with boletos in memory, a seed and failures injected through /control.
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

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/config"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
	"github.com/gengibrepower/cross-border-pay/internal/platform/logging"
	"github.com/gengibrepower/cross-border-pay/internal/providers/boletosim/server"
)

const (
	defaultAddr = ":8092"
	defaultSeed = 1
	// defaultBank is no bank's COMPE code: a boleto of the simulator is paid only in it.
	defaultBank        = "999"
	defaultBeneficiary = "00012345"
	defaultWebhookURL  = "http://localhost:8080/v1/providers/boletosim/events"
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
	s := settings{
		addr: defaultAddr,
		sim: server.Config{
			Seed: defaultSeed, Bank: defaultBank, Beneficiary: defaultBeneficiary, WebhookURL: defaultWebhookURL,
		},
	}
	var errs []error
	if v := getenv("BOLETOSIM_ADDR"); v != "" {
		if _, _, err := net.SplitHostPort(v); err != nil {
			errs = append(errs, fmt.Errorf("BOLETOSIM_ADDR: %w", err))
		}
		s.addr = v
	}
	if v := getenv("BOLETOSIM_SEED"); v != "" {
		seed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("BOLETOSIM_SEED: %w", err))
		}
		s.sim.Seed = seed
	}
	if v := getenv("BOLETOSIM_BANK"); v != "" {
		s.sim.Bank = v
	}
	if v := getenv("BOLETOSIM_BENEFICIARY"); v != "" {
		s.sim.Beneficiary = v
	}
	if v := getenv("BOLETOSIM_WEBHOOK_URL"); v != "" {
		s.sim.WebhookURL = v
	}
	s.sim.WebhookSecret = []byte(getenv("BOLETOSIM_WEBHOOK_SECRET"))
	if len(s.sim.WebhookSecret) == 0 {
		errs = append(errs, errors.New("BOLETOSIM_WEBHOOK_SECRET is required: it signs the events sent to BOLETOSIM_WEBHOOK_URL"))
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

	cal, err := calendar.Brazil()
	if err != nil {
		return fmt.Errorf("calendar: %w", err)
	}
	sim, err := server.New(&s.sim, clock.System{}, cal, logger)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	sim.Register(mux)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	logger.Info("boleto simulator configured", "seed", s.sim.Seed, "bank", s.sim.Bank, "calendar", cal.Version(), "webhook", s.sim.WebhookURL)
	if err := httpapi.Serve(ctx, "boletosim", ln, httpapi.Handler(mux, logger), logger, cfg.ShutdownTimeout); err != nil {
		return fmt.Errorf("http: %w", err)
	}
	return nil
}
