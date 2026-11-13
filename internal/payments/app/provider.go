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
	// ErrLiveAttempt is a second attempt for a payment whose last one was not rejected.
	ErrLiveAttempt = errors.New("payments: payment already has a live attempt")
	// ErrNotAtProvider is GetAttempt finding no attempt with that ID: the provider never
	// received it, or has not yet.
	ErrNotAtProvider = errors.New("payments: provider has no such attempt")
	// ErrUnknownEventType is a provider event payments does not act on.
	ErrUnknownEventType = errors.New("payments: event type not handled")
	// ErrEventDivergent is a provider event that contradicts what payments knows, such
	// as another amount or a payment for a rejected attempt: it can never be applied.
	ErrEventDivergent = errors.New("payments: event diverges from the payment")
)

// Provider is the port of section 4.3.
//
// A returned error never means the provider refused: a refusal is a result with
// Rejected. Any error is ambiguous, because the provider may have acted before it
// failed, and the attempt or refund goes to unknown.
type Provider interface {
	Name() domain.ProviderName
	CreateAttempt(ctx context.Context, req *AttemptRequest) (AttemptResult, error)
	// GetAttempt looks the attempt up by our ID, which CreateAttempt sent as the
	// provider's idempotency key. It returns ErrNotAtProvider when there is none.
	GetAttempt(ctx context.Context, id domain.AttemptID) (AttemptResult, error)
	// Refund returns money of a paid attempt to the payer, with our refund ID as the
	// provider's idempotency key.
	Refund(ctx context.Context, req *RefundRequest) (RefundResult, error)
	// GetRefund looks the refund up by our ID. It returns ErrNotAtProvider when there
	// is none.
	GetRefund(ctx context.Context, id domain.RefundID) (RefundResult, error)
}

// Capturer is a Provider whose attempts can be authorized now and captured later, a
// card acquirer (decision 18).
type Capturer interface {
	// Capture takes the amount of an accepted attempt. A refusal, such as an
	// authorization that expired, is ErrCaptureRefused; any other error is ambiguous.
	Capture(ctx context.Context, req *CaptureRequest) (AttemptResult, error)
	// Void releases an accepted attempt nothing was captured of, so the payer's limit
	// comes back at once instead of when the authorization expires.
	Void(ctx context.Context, req *CaptureRequest) error
}

// ErrCaptureRefused is the acquirer saying no to a capture: nothing was charged.
var ErrCaptureRefused = errors.New("payments: provider refused the capture")

type CaptureRequest struct {
	Attempt domain.AttemptID
	// Authorization is the provider's ID of the attempt, its ExternalID.
	Authorization string
	// Amount is what the capture takes, in the payer's currency; zero for a void.
	Amount money.Money
}

type RefundRequest struct {
	Refund domain.RefundID
	// Charge is the provider's ID of the paid attempt, its ExternalID.
	Charge string
	// Amount is what goes back to the payer, in the payer's currency.
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
	// Reason is the provider's words for a rejection, kept for the operator.
	Reason string
}

type AttemptRequest struct {
	Attempt domain.AttemptID
	Payment domain.PaymentID
	Method  domain.Method
	// Amount is what the payer pays, in the payer's currency.
	Amount    money.Money
	ExpiresAt time.Time
	// Card is the payer's card, set for a card attempt.
	Card domain.CardInput
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
	// Pix is set when an accepted PIX attempt returns its BR Code. A card attempt
	// needs no instructions: the payer already gave the card.
	Pix *PixInstructions
	// PaidAt and Amount are when and how much the payer paid, for Succeeded.
	PaidAt time.Time
	Amount money.Money
}

type PixInstructions struct {
	BRCode string
}

// Update is the result as a change to the attempt.
func (r *AttemptResult) Update() domain.AttemptUpdate {
	u := domain.AttemptUpdate{Event: r.Event(), ExternalID: r.ExternalID, PaidAt: r.PaidAt, Amount: r.Amount}
	if r.Pix != nil {
		u.BRCode = r.Pix.BRCode
	}
	return u
}

// Event is what the result does to the attempt. An outcome the adapter left unset is
// ambiguous, not a refusal: the safe reading of a result nobody understood.
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
