package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	fxadapters "github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	fxapp "github.com/gengibrepower/cross-border-pay/internal/fx/app"
	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
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
	payments *paymentsadapters.Handler
	auth     func(http.Handler) http.Handler
}

func newAPI(pool *pgxpool.Pool, logger *slog.Logger, usdbrl string, clock fxapp.Clock) (*api, error) {
	rate, err := fx.ParseRate(money.USD, money.BRL, usdbrl)
	if err != nil {
		return nil, fmt.Errorf("config: FX_USD_BRL_RATE: %w", err)
	}
	quoter := fxapp.NewQuoter(fxadapters.NewFixedRates(rate), clock, fx.DefaultPricing)
	merchants := merchantsapp.NewService(merchantsadapters.NewPostgres(pool), rand.Reader)

	return &api{
		payments: paymentsadapters.NewHandler(
			paymentsapp.NewService(quoter, paymentsadapters.NewPostgres(pool)),
			merchantFrom,
			logger,
		),
		auth: merchantsadapters.RequireAPIKey(merchants, logger),
	}, nil
}

func (a *api) register(mux *http.ServeMux) {
	a.payments.Register(mux, a.auth)
}

// merchantFrom is the bridge that keeps payments from importing merchants.
func merchantFrom(ctx context.Context) (payments.MerchantID, bool) {
	p, ok := merchantsadapters.PrincipalFrom(ctx)
	return payments.MerchantID(p.MerchantID), ok
}
