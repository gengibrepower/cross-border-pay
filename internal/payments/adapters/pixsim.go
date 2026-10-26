package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

const PixSimName domain.ProviderName = "pixsim"

// PixSim is the app.Provider of the PIX simulator. Only pixsim.ErrRejected becomes a
// result; every other error is returned, which the caller reads as unknown.
type PixSim struct {
	client *pixsim.Client
}

func NewPixSim(client *pixsim.Client) *PixSim {
	return &PixSim{client: client}
}

func (*PixSim) Name() domain.ProviderName { return PixSimName }

func (p *PixSim) CreateAttempt(ctx context.Context, req *app.AttemptRequest) (app.AttemptResult, error) {
	if req.Method != domain.Pix {
		return app.AttemptResult{}, fmt.Errorf("pixsim: method %s is not PIX", req.Method)
	}
	ch, err := p.client.Create(ctx, req.Attempt.String(), pixsim.CreateCharge{Amount: req.Amount, ExpiresAt: req.ExpiresAt})
	if errors.Is(err, pixsim.ErrRejected) {
		return app.AttemptResult{Outcome: app.Rejected}, nil
	}
	if err != nil {
		return app.AttemptResult{}, fmt.Errorf("pixsim: create attempt %s: %w", req.Attempt, err)
	}
	return result(&ch)
}

func (p *PixSim) GetAttempt(ctx context.Context, id domain.AttemptID) (app.AttemptResult, error) {
	ch, err := p.client.Get(ctx, id.String())
	if errors.Is(err, pixsim.ErrNotFound) {
		return app.AttemptResult{}, fmt.Errorf("pixsim: get attempt %s: %w: %w", id, app.ErrNotAtProvider, err)
	}
	if err != nil {
		return app.AttemptResult{}, fmt.Errorf("pixsim: get attempt %s: %w", id, err)
	}
	return result(&ch)
}

func result(ch *pixsim.Charge) (app.AttemptResult, error) {
	r := app.AttemptResult{ExternalID: ch.ID}
	switch ch.Status {
	case pixsim.Active:
		r.Outcome = app.Accepted
	case pixsim.Paid:
		r.Outcome = app.Succeeded
	case pixsim.Rejected:
		r.Outcome = app.Rejected
		return r, nil
	default:
		return app.AttemptResult{}, fmt.Errorf("pixsim: charge %s has unknown status %q", ch.ID, ch.Status)
	}
	if ch.BRCode == "" {
		return app.AttemptResult{}, fmt.Errorf("pixsim: charge %s came without a BR Code", ch.ID)
	}
	r.Pix = &app.PixInstructions{BRCode: ch.BRCode}
	return r, nil
}
