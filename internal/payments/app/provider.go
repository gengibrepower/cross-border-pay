package app

import (
	"context"
	"errors"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

var (
	ErrAttemptNotFound = errors.New("payments: attempt not found")
	ErrLiveAttempt     = errors.New("payments: payment already has a live attempt")
	// ErrNotAtProvider: the provider never received it, or has not yet.
	ErrNotAtProvider    = errors.New("payments: provider has no such attempt")
	ErrUnknownEventType = errors.New("payments: event type not handled")
	// ErrEventDivergent can never be applied, such as another amount or a payment for a
	// rejected attempt.
	ErrEventDivergent = errors.New("payments: event diverges from the payment")
	// ErrEventEarly is retried, as ErrAttemptNotFound: a chargeback of a capture still
	// being confirmed, or the close of one whose opening is still in the inbox.
	ErrEventEarly = errors.New("payments: event arrived before what it is about")
)

// Provider is the port of section 4.3.
//
// A returned error never means the provider refused: a refusal is a result with
// Rejected. Any error is ambiguous, because the provider may have acted before it
// failed, and the attempt or refund goes to unknown.
type Provider interface {
	Name() domain.ProviderName
	CreateAttempt(ctx context.Context, req *AttemptRequest) (AttemptResult, error)
	// GetAttempt looks up by our ID, which CreateAttempt sent as the idempotency key.
	GetAttempt(ctx context.Context, id domain.AttemptID) (AttemptResult, error)
	// Refund sends our refund ID as the idempotency key.
	Refund(ctx context.Context, req *RefundRequest) (RefundResult, error)
	GetRefund(ctx context.Context, id domain.RefundID) (RefundResult, error)
}

// Capturer is a card acquirer (decision 18).
type Capturer interface {
	// Capture: a refusal is ErrCaptureRefused; any other error is ambiguous.
	Capture(ctx context.Context, req *CaptureRequest) (AttemptResult, error)
	// Void gives the payer's limit back at once instead of when the authorization expires.
	Void(ctx context.Context, req *CaptureRequest) error
}

// ErrCaptureRefused: nothing was charged.
var ErrCaptureRefused = errors.New("payments: provider refused the capture")

type CaptureRequest struct {
	Attempt       domain.AttemptID
	Authorization string
	// Amount is in the payer's currency, zero for a void.
	Amount money.Money
}

type RefundRequest struct {
	Refund domain.RefundID
	Charge string
	// Amount is in the payer's currency.
	Amount money.Money
}

type RefundOutcome int

const (
	RefundSucceeded RefundOutcome = iota + 1
	RefundRejected
)

type RefundResult struct {
	Outcome    RefundOutcome
	ExternalID string
	Amount     money.Money
	Reason     string
}

type AttemptRequest struct {
	Attempt domain.AttemptID
	Payment domain.PaymentID
	Method  domain.Method
	// Amount is in the payer's currency.
	Amount    money.Money
	ExpiresAt time.Time
	Card      domain.CardInput
	Boleto    domain.BoletoInput
}

type Outcome int

const (
	Accepted Outcome = iota + 1
	Rejected
	Succeeded
)

type AttemptResult struct {
	Outcome    Outcome
	ExternalID string
	// A card attempt needs no instructions: the payer already gave the card.
	Pix    *PixInstructions
	Boleto *BoletoInstructions
	PaidAt time.Time
	Amount money.Money
}

type PixInstructions struct {
	BRCode string
}

type BoletoInstructions struct {
	Barcode string
}

func (r *AttemptResult) Update() domain.AttemptUpdate {
	u := domain.AttemptUpdate{Event: r.Event(), ExternalID: r.ExternalID, PaidAt: r.PaidAt, Amount: r.Amount}
	if r.Pix != nil {
		u.BRCode = r.Pix.BRCode
	}
	if r.Boleto != nil {
		u.Barcode = r.Boleto.Barcode
	}
	return u
}

// Event reads an unset outcome as ambiguous, not a refusal: the safe reading of a
// result nobody understood.
func (r *AttemptResult) Event() domain.AttemptEvent {
	switch r.Outcome {
	case Accepted:
		return domain.Accept
	case Rejected:
		return domain.Reject
	case Succeeded:
		return domain.Succeed
	default:
		return domain.Ambiguous
	}
}
