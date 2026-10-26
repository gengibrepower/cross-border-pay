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
)

// Provider is the port of section 4.3. Refund joins it in F7, when refunds exist.
//
// A returned error never means the provider refused: a refusal is a result with
// Rejected. Any error is ambiguous, because the provider may have acted before it
// failed, and the attempt goes to unknown.
type Provider interface {
	Name() domain.ProviderName
	CreateAttempt(ctx context.Context, req *AttemptRequest) (AttemptResult, error)
	// GetAttempt looks the attempt up by our ID, which CreateAttempt sent as the
	// provider's idempotency key.
	GetAttempt(ctx context.Context, id domain.AttemptID) (AttemptResult, error)
}

type AttemptRequest struct {
	Attempt domain.AttemptID
	Payment domain.PaymentID
	Method  domain.Method
	// Amount is what the payer pays, in the payer's currency.
	Amount    money.Money
	ExpiresAt time.Time
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
	// Pix is set when an accepted PIX attempt returns its BR Code. Boleto and card get
	// fields of their own in F8.
	Pix *PixInstructions
}

type PixInstructions struct {
	BRCode string
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
