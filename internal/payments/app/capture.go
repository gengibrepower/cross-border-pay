package app

import (
	"context"
	"errors"
	"fmt"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

var (
	// ErrNotCapturable is a payment with no authorization to capture: not a card, not
	// authorized, declined, already closed, or refused by the acquirer.
	ErrNotCapturable = errors.New("payments: payment cannot be captured")
	// ErrCaptureInProgress is a capture whose earlier answer was lost: the resolver
	// finds out what happened before anything else is tried.
	ErrCaptureInProgress = errors.New("payments: a capture of this payment is still unresolved")
	// ErrCaptureConflict is a capture of another amount than the one already started or
	// taken: an authorization is captured once.
	ErrCaptureConflict = errors.New("payments: payment is already captured for another amount")
	// ErrAuthorizationExpired is a capture after the authorization's validity.
	ErrAuthorizationExpired = errors.New("payments: authorization expired")
)

// PreparedCapture is a capture ready for the acquirer, or Done when the payment was
// already captured for that amount and the call is a retry to answer as it is.
type PreparedCapture struct {
	Attempt domain.PaymentAttempt
	Quote   fx.Quote
	Done    bool
}

// Capture takes amount, the whole payment when nil, of an authorized card payment
// (decision 18). The quote of what it takes is stored before the acquirer is called,
// so the capture is the same however often it is retried, and so a capture whose
// answer was lost is confirmed against it by the resolver. A capture is final: the
// rest of the authorization is released, and refunds stop at what was captured.
func (s *Service) Capture(ctx context.Context, merchant domain.MerchantID, id domain.PaymentID, amount *money.Money) (Priced, error) {
	c, err := s.store.PrepareCapture(ctx, merchant, id, amount, s.clock.Now())
	if err != nil {
		return Priced{}, fmt.Errorf("payments: capture %s: %w", id, err)
	}
	if !c.Done {
		if err := s.capture(context.WithoutCancel(ctx), &c); err != nil {
			return Priced{}, fmt.Errorf("payments: capture %s: %w", id, err)
		}
	}
	return s.Get(context.WithoutCancel(ctx), merchant, id)
}

func (s *Service) capture(ctx context.Context, c *PreparedCapture) error {
	capturer, err := s.capturer(&c.Attempt)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, s.providerTimeout)
	result, err := capturer.Capture(callCtx, &CaptureRequest{
		Attempt: c.Attempt.ID(), Authorization: c.Attempt.ExternalID(), Amount: c.Quote.PayerAmount(),
	})
	cancel()
	u := result.Update()
	switch {
	case errors.Is(err, ErrCaptureRefused):
		return fmt.Errorf("%w: %w", ErrNotCapturable, err)
	case err != nil:
		s.logger.WarnContext(ctx, "capture answer is ambiguous; attempt goes to unknown",
			"attempt", c.Attempt.ID().String(), "error", err)
		u = domain.AttemptUpdate{Event: domain.Ambiguous}
	}
	if _, err := s.apply(ctx, &c.Attempt, &u); err != nil {
		return err
	}
	return nil
}

func (s *Service) capturer(a *domain.PaymentAttempt) (Capturer, error) {
	provider, err := s.providers.Named(a.Provider())
	if err != nil {
		return nil, err
	}
	capturer, ok := provider.(Capturer)
	if !ok {
		return nil, fmt.Errorf("%w: %s does not capture", ErrNotCapturable, a.Provider())
	}
	return capturer, nil
}

// void releases a canceled payment's authorization. It is best effort: an
// authorization not voided lapses on its own when it expires, and no capture of a
// canceled payment is ever asked for.
func (s *Service) void(ctx context.Context, a *domain.PaymentAttempt) {
	if a.Status() != domain.AttemptAccepted || !a.Card().ManualCapture {
		return
	}
	capturer, err := s.capturer(a)
	if err == nil {
		callCtx, cancel := context.WithTimeout(ctx, s.providerTimeout)
		err = capturer.Void(callCtx, &CaptureRequest{Attempt: a.ID(), Authorization: a.ExternalID()})
		cancel()
	}
	if err != nil {
		s.logger.WarnContext(ctx, "void failed; the authorization lapses when it expires",
			"attempt", a.ID().String(), "error", err)
	}
}
