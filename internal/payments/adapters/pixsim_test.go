package adapters_test

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/brcode"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim/server"
)

// The adapter must satisfy the port; this fails to compile otherwise.
var _ app.Provider = (*adapters.PixSim)(nil)

func newPixSim(t *testing.T) (*adapters.PixSim, *pixsim.Client) {
	t.Helper()
	return newPixSimWith(t, clock.NewFake(t0))
}

// newPixSimWith runs the simulator on c, so a test controls when the payer pays.
func newPixSimWith(t *testing.T, c *clock.Fake) (*adapters.PixSim, *pixsim.Client) {
	t.Helper()
	sim, err := server.New(&server.Config{
		Seed: 1, PixKey: "123e4567-e12b-12d1-a456-426655440000", MerchantName: "CROSS BORDER PAY", MerchantCity: "SAO PAULO",
	}, c, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	sim.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := pixsim.NewClient(srv.URL, &http.Client{Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return adapters.NewPixSim(client), client
}

func attemptRequest(id byte) *app.AttemptRequest {
	return &app.AttemptRequest{
		Attempt: domain.AttemptID{id}, Payment: domain.PaymentID{id}, Method: domain.Pix,
		Amount: money.New(10150, money.BRL), ExpiresAt: t0.Add(domain.PixQuoteValidity),
	}
}

func TestPixSimAcceptsThenSucceeds(t *testing.T) {
	p, client := newPixSim(t)
	req := attemptRequest(1)

	created, err := p.CreateAttempt(t.Context(), req)
	if err != nil || created.Outcome != app.Accepted || created.ExternalID == "" || created.Pix == nil {
		t.Fatalf("CreateAttempt() = %+v, %v; want accepted with a BR Code", created, err)
	}
	if code, err := brcode.Parse(created.Pix.BRCode); err != nil || code.Amount != req.Amount {
		t.Errorf("BR Code = %+v, %v; want amount %s", code, err, req.Amount)
	}
	if _, err := client.Pay(t.Context(), created.ExternalID); err != nil {
		t.Fatal(err)
	}
	got, err := p.GetAttempt(t.Context(), req.Attempt)
	if err != nil || got.Outcome != app.Succeeded || got.ExternalID != created.ExternalID {
		t.Errorf("GetAttempt() = %+v, %v; want succeeded %s", got, err, created.ExternalID)
	}
}

func TestPixSimRejectionIsAResult(t *testing.T) {
	p, client := newPixSim(t)
	if err := client.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpCreate, Kind: pixsim.Reject, Count: 1}); err != nil {
		t.Fatal(err)
	}
	r, err := p.CreateAttempt(t.Context(), attemptRequest(1))
	if err != nil || r.Event() != domain.Reject {
		t.Fatalf("CreateAttempt() = %+v, %v; want a rejection without error", r, err)
	}
	got, err := p.GetAttempt(t.Context(), domain.AttemptID{1})
	if err != nil || got.Outcome != app.Rejected || got.ExternalID == "" {
		t.Errorf("GetAttempt() = %+v, %v; want rejected", got, err)
	}
}

// Every failure other than a refusal is an error, which the caller records as unknown.
func TestPixSimFailuresAreErrors(t *testing.T) {
	for _, kind := range []pixsim.FaultKind{pixsim.FailBefore, pixsim.TimeoutAfter} {
		t.Run(string(kind), func(t *testing.T) {
			p, client := newPixSim(t)
			if err := client.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpCreate, Kind: kind, Count: 1}); err != nil {
				t.Fatal(err)
			}
			if r, err := p.CreateAttempt(t.Context(), attemptRequest(1)); err == nil {
				t.Errorf("CreateAttempt() = %+v, want an error", r)
			}
		})
	}

	p, _ := newPixSim(t)
	if _, err := p.GetAttempt(t.Context(), domain.AttemptID{9}); !errors.Is(err, pixsim.ErrNotFound) {
		t.Errorf("GetAttempt(unknown) error = %v, want %v", err, pixsim.ErrNotFound)
	}
	boleto := attemptRequest(2)
	boleto.Method = domain.Boleto
	if _, err := p.CreateAttempt(t.Context(), boleto); err == nil {
		t.Error("CreateAttempt(boleto) succeeded")
	}
}

func TestPixSimRefund(t *testing.T) {
	p, client := newPixSim(t)
	created, err := p.CreateAttempt(t.Context(), attemptRequest(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Pay(t.Context(), created.ExternalID); err != nil {
		t.Fatal(err)
	}
	req := &app.RefundRequest{Refund: domain.RefundID{1}, Charge: created.ExternalID, Amount: money.New(5075, money.BRL)}
	r, err := p.Refund(t.Context(), req)
	if err != nil || r.Outcome != app.RefundSucceeded || r.ExternalID == "" || r.Amount != req.Amount {
		t.Fatalf("Refund() = %+v, %v; want succeeded for %s", r, err, req.Amount)
	}
	if got, err := p.GetRefund(t.Context(), req.Refund); err != nil || got != r {
		t.Errorf("GetRefund() = %+v, %v; want %+v", got, err, r)
	}

	over := &app.RefundRequest{Refund: domain.RefundID{2}, Charge: created.ExternalID, Amount: money.New(5076, money.BRL)}
	if r, err := p.Refund(t.Context(), over); err != nil || r.Outcome != app.RefundRejected {
		t.Errorf("Refund() over what is left = %+v, %v; want a rejection without error", r, err)
	}
	if got, err := p.GetRefund(t.Context(), over.Refund); err != nil || got.Outcome != app.RefundRejected || got.Reason == "" {
		t.Errorf("GetRefund() = %+v, %v; want rejected with the reason", got, err)
	}

	if err := client.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.TimeoutAfter, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if r, err := p.Refund(t.Context(), &app.RefundRequest{Refund: domain.RefundID{3}, Charge: created.ExternalID, Amount: money.New(1, money.BRL)}); err == nil {
		t.Errorf("Refund() that timed out = %+v, want an error", r)
	}
	if _, err := p.GetRefund(t.Context(), domain.RefundID{9}); !errors.Is(err, app.ErrNotAtProvider) {
		t.Errorf("GetRefund(unknown) error = %v, want %v", err, app.ErrNotAtProvider)
	}
}
