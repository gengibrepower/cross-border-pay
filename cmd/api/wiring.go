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

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
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
	"github.com/gengibrepower/cross-border-pay/internal/platform/ssrf"
	"github.com/gengibrepower/cross-border-pay/internal/providers/boletosim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
	reconciliationadapters "github.com/gengibrepower/cross-border-pay/internal/reconciliation/adapters"
	reconciliation "github.com/gengibrepower/cross-border-pay/internal/reconciliation/domain"
	webhooksadapters "github.com/gengibrepower/cross-border-pay/internal/webhooks/adapters"
	webhooksapp "github.com/gengibrepower/cross-border-pay/internal/webhooks/app"
)

// api is the only place that knows how the modules fit together.
type api struct {
	payments   *paymentsadapters.Handler
	events     *eventsadapters.Handler
	endpoints  *merchantsadapters.EndpointHandler
	outbox     *webhooksadapters.EventsHandler
	auth       func(http.Handler) http.Handler
	idempotent func(http.Handler) http.Handler
}

// deps are what tests replace.
type deps struct {
	usdbrl          string
	clock           moduleClock
	calendar        *calendar.Calendar
	providers       *paymentsapp.Providers
	disputers       map[payments.ProviderName]paymentsapp.Disputer
	providerTimeout time.Duration
	webhookSecret   string
	boletoSecret    string
	cardSecret      string
	signingKey      []byte
	webhooks        ssrf.Policy
}

// moduleClock is every module's Clock at once, so one clock drives them all.
type moduleClock interface {
	fxapp.Clock
	paymentsapp.Clock
	idempotencyapp.Clock
	eventsapp.Clock
	merchantsapp.Clock
	webhooksapp.Clock
}

func newAPI(pool *pgxpool.Pool, logger *slog.Logger, d *deps) (*api, error) {
	rate, err := fx.ParseRate(money.USD, money.BRL, d.usdbrl)
	if err != nil {
		return nil, fmt.Errorf("config: FX_USD_BRL_RATE: %w", err)
	}
	rates := fxadapters.NewFixedRates(rate)
	quoter := fxapp.NewQuoter(rates, d.clock, fx.DefaultPricing)
	sinks := paymentSinks()
	sinks.Rates = rates
	paymentsStore := paymentsadapters.NewPostgres(pool, sinks)
	merchantsStore := merchantsadapters.NewPostgres(pool)
	merchants := merchantsapp.NewService(merchantsStore, rand.Reader)
	policy, err := payments.DefaultSettlementPolicy()
	if err != nil {
		return nil, fmt.Errorf("settlement: %w", err)
	}
	endpoints, err := merchantsapp.NewEndpoints(merchantsStore, d.webhooks, d.signingKey, d.clock)
	if err != nil {
		return nil, fmt.Errorf("config: WEBHOOK_SIGNING_KEY: %w", err)
	}

	return &api{
		payments: paymentsadapters.NewHandler(
			paymentsapp.NewService(quoter, paymentsStore, d.providers, d.calendar, d.clock, logger, d.providerTimeout),
			paymentsapp.NewRefunder(rates, paymentsStore, d.providers, d.clock, logger, d.providerTimeout),
			paymentsapp.NewPayouts(paymentsStore, policy, d.clock),
			paymentsapp.NewChargebacks(paymentsStore, d.disputers, d.clock, logger, d.providerTimeout),
			merchantFrom,
			logger,
		),
		events: eventsadapters.NewHandler(
			eventsapp.NewInbox(eventsadapters.NewPostgres(pool), map[string]eventsapp.Verifier{
				string(paymentsadapters.PixSimName):    pixsimVerifier{secret: []byte(d.webhookSecret)},
				string(paymentsadapters.BoletoSimName): boletosimVerifier{secret: []byte(d.boletoSecret)},
				string(paymentsadapters.CardSimName):   cardsimVerifier{secret: []byte(d.cardSecret)},
			}, d.clock, logger),
			logger,
		),
		endpoints: merchantsadapters.NewEndpointHandler(endpoints, logger),
		outbox: webhooksadapters.NewEventsHandler(
			webhooksapp.NewEvents(eventsadapters.NewDeliveries(pool), merchantsStore, d.clock), logger,
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
	a.endpoints.Register(mux, a.auth)
	a.outbox.Register(mux, a.auth)
}

// pixsimVerifier keeps events from importing the simulator.
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

type boletosimVerifier struct {
	secret []byte
}

func (v boletosimVerifier) Verify(header func(string) string, body []byte, now time.Time) (events.Incoming, error) {
	if len(v.secret) == 0 {
		return events.Incoming{}, fmt.Errorf("%w: no secret configured for boletosim", eventsapp.ErrUnauthenticated)
	}
	sig := header(boletosim.SignatureHeader)
	e, err := boletosim.VerifyEvent(v.secret, sig, body, now)
	switch {
	case errors.Is(err, boletosim.ErrBadEvent):
		return events.Incoming{}, fmt.Errorf("%w: %w", eventsapp.ErrMalformedEvent, err)
	case err != nil:
		return events.Incoming{}, fmt.Errorf("%w: %w", eventsapp.ErrUnauthenticated, err)
	}
	return events.Incoming{ExternalID: e.ID, Type: string(e.Type), Payload: body, Signature: sig}, nil
}

type cardsimVerifier struct {
	secret []byte
}

func (v cardsimVerifier) Verify(header func(string) string, body []byte, now time.Time) (events.Incoming, error) {
	if len(v.secret) == 0 {
		return events.Incoming{}, fmt.Errorf("%w: no secret configured for cardsim", eventsapp.ErrUnauthenticated)
	}
	sig := header(cardsim.SignatureHeader)
	e, err := cardsim.VerifyEvent(v.secret, sig, body, now)
	switch {
	case errors.Is(err, cardsim.ErrBadEvent):
		return events.Incoming{}, fmt.Errorf("%w: %w", eventsapp.ErrMalformedEvent, err)
	case err != nil:
		return events.Incoming{}, fmt.Errorf("%w: %w", eventsapp.ErrUnauthenticated, err)
	}
	return events.Incoming{ExternalID: e.ID, Type: string(e.Type), Payload: body, Signature: sig}, nil
}

// merchantFrom keeps payments from importing merchants.
func merchantFrom(ctx context.Context) (payments.MerchantID, bool) {
	p, ok := merchantsadapters.PrincipalFrom(ctx)
	return payments.MerchantID(p.MerchantID), ok
}

func idempotencyMerchant(ctx context.Context) (idempotency.MerchantID, bool) {
	p, ok := merchantsadapters.PrincipalFrom(ctx)
	return idempotency.MerchantID(p.MerchantID), ok
}

// merchantOutbox keeps payments from importing events.
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

// reviewQueue keeps payments from importing reconciliation.
type reviewQueue struct {
	reviews reconciliationadapters.Reviews
}

func (r reviewQueue) Open(ctx context.Context, tx pgx.Tx, c *payments.ReviewCase) (bool, error) {
	payment, attempt := [16]byte(c.Payment), [16]byte(c.Attempt)
	opened, err := r.reviews.Open(ctx, tx, &reconciliation.Case{
		Kind: reconciliation.CaseKind(c.Kind), Provider: string(c.Provider), ExternalID: c.ExternalID,
		Payment: &payment, Attempt: &attempt, Amount: c.Amount, LedgerReference: c.LedgerReference,
		Details: c.Details, CreatedAt: c.CreatedAt,
	})
	if err != nil {
		return false, fmt.Errorf("review queue: %w", err)
	}
	return opened, nil
}

func paymentSinks() paymentsadapters.Sinks {
	return paymentsadapters.Sinks{Outbox: merchantOutbox{}, Reviews: reviewQueue{}}
}
