package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
)

const CardSimName domain.ProviderName = "cardsim"

// CardSim is the app.Provider of the card simulator. A decline is a result, as the
// issuer's answer; every error is returned, which the caller reads as unknown.
type CardSim struct {
	client *cardsim.Client
}

func NewCardSim(client *cardsim.Client) *CardSim {
	return &CardSim{client: client}
}

func (*CardSim) Name() domain.ProviderName { return CardSimName }

// CreateAttempt authorizes, and captures at once unless the merchant captures later
// (decision 18).
func (p *CardSim) CreateAttempt(ctx context.Context, req *app.AttemptRequest) (app.AttemptResult, error) {
	if req.Method != domain.Card {
		return app.AttemptResult{}, fmt.Errorf("cardsim: method %s is not card", req.Method)
	}
	a, err := p.client.Authorize(ctx, req.Attempt.String(), cardsim.Authorize{Amount: req.Amount, Token: req.Card.Token, Capture: !req.Card.ManualCapture})
	if err != nil {
		return app.AttemptResult{}, fmt.Errorf("cardsim: create attempt %s: %w", req.Attempt, err)
	}
	return authorizationResult(&a)
}

func (p *CardSim) GetAttempt(ctx context.Context, id domain.AttemptID) (app.AttemptResult, error) {
	a, err := p.client.Get(ctx, id.String())
	if errors.Is(err, cardsim.ErrNotFound) {
		return app.AttemptResult{}, fmt.Errorf("cardsim: get attempt %s: %w: %w", id, app.ErrNotAtProvider, err)
	}
	if err != nil {
		return app.AttemptResult{}, fmt.Errorf("cardsim: get attempt %s: %w", id, err)
	}
	return authorizationResult(&a)
}

func authorizationResult(a *cardsim.Authorization) (app.AttemptResult, error) {
	r := app.AttemptResult{ExternalID: a.ID}
	switch a.Status {
	case cardsim.Authorized:
		r.Outcome = app.Accepted
	case cardsim.Captured:
		if a.CapturedAt == nil {
			return app.AttemptResult{}, fmt.Errorf("cardsim: authorization %s is captured without a time", a.ID)
		}
		r.Outcome, r.Amount, r.PaidAt = app.Succeeded, a.Captured, a.CapturedAt.UTC()
	case cardsim.Declined, cardsim.Expired, cardsim.Voided:
		// Expired and voided are authorizations nobody captured: no money moved.
		r.Outcome = app.Rejected
	default:
		return app.AttemptResult{}, fmt.Errorf("cardsim: authorization %s has unknown status %q", a.ID, a.Status)
	}
	return r, nil
}

func (p *CardSim) Capture(ctx context.Context, req *app.CaptureRequest) (app.AttemptResult, error) {
	a, err := p.client.Capture(ctx, req.Authorization, cardsim.Capture{Amount: req.Amount})
	if errors.Is(err, cardsim.ErrNotCapturable) {
		return app.AttemptResult{}, fmt.Errorf("cardsim: capture %s: %w: %w", req.Attempt, app.ErrCaptureRefused, err)
	}
	if err != nil {
		return app.AttemptResult{}, fmt.Errorf("cardsim: capture %s: %w", req.Attempt, err)
	}
	return authorizationResult(&a)
}

func (p *CardSim) Void(ctx context.Context, req *app.CaptureRequest) error {
	if _, err := p.client.Void(ctx, req.Authorization); err != nil {
		return fmt.Errorf("cardsim: void %s: %w", req.Attempt, err)
	}
	return nil
}

func (p *CardSim) Refund(ctx context.Context, req *app.RefundRequest) (app.RefundResult, error) {
	rf, err := p.client.Refund(ctx, req.Refund.String(), req.Charge, cardsim.CreateRefund{Amount: req.Amount})
	if errors.Is(err, cardsim.ErrRefundRejected) {
		return app.RefundResult{Outcome: app.RefundRejected, Amount: req.Amount, Reason: err.Error()}, nil
	}
	if err != nil {
		return app.RefundResult{}, fmt.Errorf("cardsim: refund %s: %w", req.Refund, err)
	}
	return cardRefundResult(&rf)
}

func (p *CardSim) GetRefund(ctx context.Context, id domain.RefundID) (app.RefundResult, error) {
	rf, err := p.client.GetRefund(ctx, id.String())
	if errors.Is(err, cardsim.ErrRefundNotFound) {
		return app.RefundResult{}, fmt.Errorf("cardsim: get refund %s: %w: %w", id, app.ErrNotAtProvider, err)
	}
	if err != nil {
		return app.RefundResult{}, fmt.Errorf("cardsim: get refund %s: %w", id, err)
	}
	return cardRefundResult(&rf)
}

func cardRefundResult(rf *cardsim.Refund) (app.RefundResult, error) {
	r := app.RefundResult{ExternalID: rf.ID, Amount: rf.Amount, Reason: rf.Reason}
	switch rf.Status {
	case cardsim.RefundCompleted:
		r.Outcome = app.RefundSucceeded
	case cardsim.RefundRejected:
		r.Outcome = app.RefundRejected
	default:
		return app.RefundResult{}, fmt.Errorf("cardsim: refund %s has unknown status %q", rf.ID, rf.Status)
	}
	return r, nil
}
