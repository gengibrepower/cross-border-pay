package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/boletosim"
)

const BoletoSimName domain.ProviderName = "boletosim"

// BoletoSim is the app.Provider of the boleto simulator. Only boletosim.ErrRejected
// becomes a result; every other error is returned, which the caller reads as unknown.
type BoletoSim struct {
	client *boletosim.Client
}

func NewBoletoSim(client *boletosim.Client) *BoletoSim {
	return &BoletoSim{client: client}
}

func (*BoletoSim) Name() domain.ProviderName { return BoletoSimName }

func (p *BoletoSim) CreateAttempt(ctx context.Context, req *app.AttemptRequest) (app.AttemptResult, error) {
	if req.Method != domain.Boleto {
		return app.AttemptResult{}, fmt.Errorf("boletosim: method %s is not boleto", req.Method)
	}
	in := &req.Boleto
	if in.Due.IsZero() || in.Payer.Document.IsZero() {
		return app.AttemptResult{}, fmt.Errorf("boletosim: attempt %s has no payer or due date", req.Attempt)
	}
	b, err := p.client.Create(ctx, req.Attempt.String(), &boletosim.CreateBoleto{
		Amount: req.Amount, DueDate: in.Due,
		Payer: boletosim.Payer{Name: in.Payer.Name, Document: in.Payer.Document.Digits(), Email: in.Payer.Email},
	})
	if errors.Is(err, boletosim.ErrRejected) {
		return app.AttemptResult{Outcome: app.Rejected}, nil
	}
	if err != nil {
		return app.AttemptResult{}, fmt.Errorf("boletosim: create attempt %s: %w", req.Attempt, err)
	}
	return boletoResult(&b)
}

func (p *BoletoSim) GetAttempt(ctx context.Context, id domain.AttemptID) (app.AttemptResult, error) {
	b, err := p.client.Get(ctx, id.String())
	if errors.Is(err, boletosim.ErrNotFound) {
		return app.AttemptResult{}, fmt.Errorf("boletosim: get attempt %s: %w: %w", id, app.ErrNotAtProvider, err)
	}
	if err != nil {
		return app.AttemptResult{}, fmt.Errorf("boletosim: get attempt %s: %w", id, err)
	}
	return boletoResult(&b)
}

// boletoResult reads an expired boleto as accepted: it was registered and nobody paid
// it, so the attempt stands as it was and the payment expires by its own quote, which
// ends with the boleto's last day (decision 44).
func boletoResult(b *boletosim.Boleto) (app.AttemptResult, error) {
	r := app.AttemptResult{ExternalID: b.ID}
	switch b.Status {
	case boletosim.Registered, boletosim.Expired:
		r.Outcome = app.Accepted
	case boletosim.Paid:
		if b.PaidAt == nil {
			return app.AttemptResult{}, fmt.Errorf("boletosim: boleto %s is paid without a time", b.ID)
		}
		r.Outcome, r.Amount, r.PaidAt = app.Succeeded, b.Amount, b.PaidAt.UTC()
	case boletosim.Rejected:
		r.Outcome = app.Rejected
		return r, nil
	default:
		return app.AttemptResult{}, fmt.Errorf("boletosim: boleto %s has unknown status %q", b.ID, b.Status)
	}
	if b.Barcode == "" {
		return app.AttemptResult{}, fmt.Errorf("boletosim: boleto %s came without a barcode", b.ID)
	}
	r.Boleto = &app.BoletoInstructions{Barcode: b.Barcode}
	return r, nil
}

func (p *BoletoSim) Refund(ctx context.Context, req *app.RefundRequest) (app.RefundResult, error) {
	rf, err := p.client.Refund(ctx, req.Refund.String(), req.Charge, boletosim.CreateRefund{Amount: req.Amount})
	if errors.Is(err, boletosim.ErrRefundRejected) {
		return app.RefundResult{Outcome: app.RefundRejected, Amount: req.Amount, Reason: err.Error()}, nil
	}
	if err != nil {
		return app.RefundResult{}, fmt.Errorf("boletosim: refund %s: %w", req.Refund, err)
	}
	return boletoRefundResult(&rf)
}

func (p *BoletoSim) GetRefund(ctx context.Context, id domain.RefundID) (app.RefundResult, error) {
	rf, err := p.client.GetRefund(ctx, id.String())
	if errors.Is(err, boletosim.ErrRefundNotFound) {
		return app.RefundResult{}, fmt.Errorf("boletosim: get refund %s: %w: %w", id, app.ErrNotAtProvider, err)
	}
	if err != nil {
		return app.RefundResult{}, fmt.Errorf("boletosim: get refund %s: %w", id, err)
	}
	return boletoRefundResult(&rf)
}

func boletoRefundResult(rf *boletosim.Refund) (app.RefundResult, error) {
	r := app.RefundResult{ExternalID: rf.ID, Amount: rf.Amount, Reason: rf.Reason}
	switch rf.Status {
	case boletosim.RefundCompleted:
		r.Outcome = app.RefundSucceeded
	case boletosim.RefundRejected:
		r.Outcome = app.RefundRejected
	default:
		return app.RefundResult{}, fmt.Errorf("boletosim: refund %s has unknown status %q", rf.ID, rf.Status)
	}
	return r, nil
}
