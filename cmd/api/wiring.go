package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	fxadapters "github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	fxapp "github.com/gengibrepower/cross-border-pay/internal/fx/app"
	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	idempotencyadapters "github.com/gengibrepower/cross-border-pay/internal/idempotency/adapters"
	idempotencyapp "github.com/gengibrepower/cross-border-pay/internal/idempotency/app"
	idempotency "github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	merchantsadapters "github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	merchantsapp "github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// api holds the /v1 routes, built from every module. This is the only place that
// knows how the modules fit together.
type api struct {
	payments   *paymentsadapters.Handler
	auth       func(http.Handler) http.Handler
	idempotent func(http.Handler) http.Handler
}

// deps are what the API needs from outside the database: the base rate, the clock and
// the provider, which tests replace.
type deps struct {
	usdbrl          string
	clock           moduleClock
	provider        paymentsapp.Provider
	providerTimeout time.Duration
}

// moduleClock is every module's Clock at once, so one clock drives them all.
type moduleClock interface {
	fxapp.Clock
	paymentsapp.Clock
	idempotencyapp.Clock
}

func newAPI(pool *pgxpool.Pool, logger *slog.Logger, d *deps) (*api, error) {
	rate, err := fx.ParseRate(money.USD, money.BRL, d.usdbrl)
	if err != nil {
		return nil, fmt.Errorf("config: FX_USD_BRL_RATE: %w", err)
	}
	quoter := fxapp.NewQuoter(fxadapters.NewFixedRates(rate), d.clock, fx.DefaultPricing)
	merchants := merchantsapp.NewService(merchantsadapters.NewPostgres(pool), rand.Reader)

	return &api{
		payments: paymentsadapters.NewHandler(
			paymentsapp.NewService(quoter, paymentsadapters.NewPostgres(pool), d.provider, d.clock, logger, d.providerTimeout),
			merchantFrom,
			logger,
		),
		auth: merchantsadapters.RequireAPIKey(merchants, logger),
		idempotent: idempotencyadapters.Require(
			idempotencyapp.NewService(idempotencyadapters.NewPostgres(pool), d.clock, rand.Reader),
			idempotencyMerchant,
			logger,
		),
	}, nil
}

func (a *api) register(mux *http.ServeMux) {
	a.payments.Register(mux, a.auth, a.idempotent)
}

// merchantFrom is the bridge that keeps payments from importing merchants.
func merchantFrom(ctx context.Context) (payments.MerchantID, bool) {
	p, ok := merchantsadapters.PrincipalFrom(ctx)
	return payments.MerchantID(p.MerchantID), ok
}

func idempotencyMerchant(ctx context.Context) (idempotency.MerchantID, bool) {
	p, ok := merchantsadapters.PrincipalFrom(ctx)
	return idempotency.MerchantID(p.MerchantID), ok
}
