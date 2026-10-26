package app

import (
	"context"
	"fmt"

	idempotency "github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

func (s *Service) createPayment(ctx context.Context, held *idempotency.Held, merchant domain.MerchantID, method string, amount money.Money, reference string) (Priced, error) {
	draft, err := domain.NewDraft(merchant, method, amount, reference)
	if err != nil {
		return Priced{}, fmt.Errorf("payments: create: %w", err)
	}
	quote, err := s.quoter.Quote(ctx, amount, money.BRL, domain.PixQuoteValidity)
	if err != nil {
		return Priced{}, fmt.Errorf("payments: create: %w", err)
	}
	p, err := s.store.Create(ctx, held, &draft, &quote, s.provider.Name())
	if err != nil {
		return Priced{}, fmt.Errorf("payments: create: %w", err)
	}
	return p, nil
}

// request takes the payment's attempt from wherever an earlier request left it to a
// stored answer, calling the provider only when it certainly was not called before.
func (s *Service) request(ctx context.Context, p *Priced) (domain.PaymentAttempt, error) {
	a := *p.Attempt
	switch a.Status() {
	case domain.AttemptCreated:
	case domain.AttemptSent:
		// An earlier request wrote sent and died before storing the answer: the
		// provider may have acted. Decision 17: find out later, never repeat blind.
		return s.apply(ctx, &a, domain.AttemptUpdate{Event: domain.Ambiguous})
	case domain.AttemptUnknown, domain.AttemptAccepted, domain.AttemptRejected, domain.AttemptSucceeded:
		return a, nil
	default:
		return domain.PaymentAttempt{}, fmt.Errorf("attempt %s has unknown status %q", a.ID(), a.Status())
	}

	// Writing sent before the call is what makes created mean "not called".
	sent, err := s.apply(ctx, &a, domain.AttemptUpdate{Event: domain.Send})
	if err != nil {
		return domain.PaymentAttempt{}, err
	}
	return s.apply(ctx, &sent, s.call(ctx, p, &sent))
}

// call asks the provider for the attempt. Any error is ambiguous (section 4.3): the
// provider may have acted before it failed, so the update is unknown, not a refusal.
func (s *Service) call(ctx context.Context, p *Priced, a *domain.PaymentAttempt) domain.AttemptUpdate {
	ctx, cancel := context.WithTimeout(ctx, s.providerTimeout)
	defer cancel()
	result, err := s.provider.CreateAttempt(ctx, &AttemptRequest{
		Attempt:   a.ID(),
		Payment:   p.Payment.ID(),
		Method:    p.Payment.Method(),
		Amount:    p.Quote.PayerAmount(),
		ExpiresAt: p.Quote.ExpiresAt(),
	})
	if err != nil {
		s.logger.WarnContext(ctx, "provider answer is ambiguous; attempt goes to unknown",
			"attempt", a.ID().String(), "provider", string(a.Provider()), "error", err)
		return domain.AttemptUpdate{Event: domain.Ambiguous}
	}
	u := domain.AttemptUpdate{Event: result.Event(), ExternalID: result.ExternalID}
	if result.Pix != nil {
		u.BRCode = result.Pix.BRCode
	}
	return u
}

func (s *Service) apply(ctx context.Context, a *domain.PaymentAttempt, u domain.AttemptUpdate) (domain.PaymentAttempt, error) {
	next, _, err := s.store.ApplyAttempt(ctx, a.ID(), u, s.clock.Now())
	if err != nil {
		return domain.PaymentAttempt{}, fmt.Errorf("apply %s: %w", u.Event, err)
	}
	return next, nil
}
