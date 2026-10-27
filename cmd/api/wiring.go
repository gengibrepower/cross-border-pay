package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	eventsadapters "github.com/gengibrepower/cross-border-pay/internal/events/adapters"
	eventsapp "github.com/gengibrepower/cross-border-pay/internal/events/app"
	events "github.com/gengibrepower/cross-border-pay/internal/events/domain"
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
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

// api holds the /v1 routes, built from every module. This is the only place that
// knows how the modules fit together.
type api struct {
	payments   *paymentsadapters.Handler
	events     *eventsadapters.Handler
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
	webhookSecret   string
}

// moduleClock is every module's Clock at once, so one clock drives them all.
type moduleClock interface {
	fxapp.Clock
	paymentsapp.Clock
	idempotencyapp.Clock
	eventsapp.Clock
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
			paymentsapp.NewService(quoter, paymentsadapters.NewPostgres(pool, merchantOutbox{}), d.provider, d.clock, logger, d.providerTimeout),
			merchantFrom,
			logger,
		),
		events: eventsadapters.NewHandler(
			eventsapp.NewInbox(eventsadapters.NewPostgres(pool), map[string]eventsapp.Verifier{
				string(paymentsadapters.PixSimName): pixsimVerifier{secret: []byte(d.webhookSecret)},
			}, d.clock, logger),
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
	a.events.Register(mux)
}

// pixsimVerifier is the bridge that keeps events from importing the simulator: it
// turns the simulator's signature errors into the inbox's.
type pixsimVerifier struct {
	secret []byte
}

func (v pixsimVerifier) Verify(header func(string) string, body []byte, now time.Time) (events.Incoming, error) {
	sig := header(pixsim.SignatureHeader)
	e, err := pixsim.VerifyEvent(v.secret, sig, body, now)
	switch {
	case errors.Is(err, pixsim.ErrBadEvent):
		return events.Incoming{}, fmt.Errorf("%w: %w", eventsapp.ErrMalformedEvent, err)
	case err != nil:
		return events.Incoming{}, fmt.Errorf("%w: %w", eventsapp.ErrUnauthenticated, err)
	}
	return events.Incoming{ExternalID: e.ID, Type: string(e.Type), Payload: body, Signature: sig}, nil
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

// merchantOutbox is the bridge that keeps payments from importing events: it hands
// payments' events to the events outbox, in payments' transaction.
type merchantOutbox struct {
	outbox eventsadapters.Outbox
}

func (o merchantOutbox) Append(ctx context.Context, tx pgx.Tx, e *payments.MerchantEvent) error {
	err := o.outbox.Append(ctx, tx, &events.Outgoing{
		ID: e.ID, Merchant: e.Merchant, Type: string(e.Type), Subject: e.Payment.String(),
		Payload: e.Payload, CreatedAt: e.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("merchant outbox: %w", err)
	}
	return nil
}
