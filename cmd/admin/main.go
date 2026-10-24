// Command admin runs operator tasks that have no public endpoint:
//
//	admin merchant create -name GameStore -mode test
//	admin key revoke -lookup <lookup>
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/config"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

var errUsage = errors.New("usage: admin merchant create -name NAME -mode test|live | admin key revoke -lookup LOOKUP")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, rand.Reader)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer, random io.Reader) error {
	if len(args) < 2 {
		return errUsage
	}
	command, flags := args[0]+" "+args[1], args[2:]

	cfg, err := config.Load(getenv)
	if err == nil {
		err = cfg.RequireDatabase()
	}
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	svc := app.NewService(adapters.NewPostgres(pool), random)

	switch command {
	case "merchant create":
		return createMerchant(ctx, svc, flags, stdout)
	case "key revoke":
		return revokeKey(ctx, svc, flags, stdout)
	default:
		return errUsage
	}
}

func createMerchant(ctx context.Context, svc *app.Service, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("merchant create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "merchant name")
	modeFlag := fs.String("mode", string(domain.Test), "test or live")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	mode, err := domain.ParseMode(*modeFlag)
	if err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}

	id, key, err := svc.CreateMerchant(ctx, *name, mode)
	if err != nil {
		return fmt.Errorf("merchant create: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "merchant %s\napi key  %s\nStore the key now: only its hash is kept and it cannot be shown again.\n", id, key); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func revokeKey(ctx context.Context, svc *app.Service, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("key revoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	lookup := fs.String("lookup", "", "the part between the mode and the secret: sk_test_<lookup>_...")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if *lookup == "" {
		return fmt.Errorf("%w: -lookup is required", errUsage)
	}
	if err := svc.RevokeKey(ctx, *lookup); err != nil {
		return fmt.Errorf("key revoke: %w", err)
	}
	if _, err := fmt.Fprintf(stdout, "revoked %s\n", *lookup); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}
