package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/platform/publicid"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

var (
	ErrInvalidAttemptID   = errors.New("payments: invalid attempt ID")
	ErrExternalIDMismatch = errors.New("payments: provider changed the attempt's external ID")
	ErrBRCodeMismatch     = errors.New("payments: provider changed the attempt's BR Code")
)

// AttemptID is also the idempotency key sent to the provider, so an attempt in unknown
// can be looked up before the provider's own ID is known.
type AttemptID [16]byte

func (id AttemptID) String() string { return publicid.Encode("att_", id) }

func ParseAttemptID(s string) (AttemptID, error) {
	id, ok := publicid.Parse("att_", s)
	if !ok {
		return AttemptID{}, fmt.Errorf("%w: %q", ErrInvalidAttemptID, s)
	}
	return id, nil
}

type ProviderName string

// PaymentAttempt is decision 6. Its status only changes through Apply.
type PaymentAttempt struct {
	id         AttemptID
	payment    PaymentID
	provider   ProviderName
	status     AttemptStatus
	externalID string
	brCode     string
	card       CardInput
	boleto     BoletoInput
	barcode    string
	createdAt  time.Time
	updatedAt  time.Time
}

// RestoreAttempt rebuilds an attempt from storage; only adapters call it.
func RestoreAttempt(id AttemptID, payment PaymentID, provider ProviderName, status AttemptStatus, externalID, brCode string, card CardInput, createdAt, updatedAt time.Time) PaymentAttempt {
	return PaymentAttempt{
		id: id, payment: payment, provider: provider, status: status,
		externalID: externalID, brCode: brCode, card: card, createdAt: createdAt, updatedAt: updatedAt,
	}
}

// RestoreBoleto completes a RestoreAttempt; only adapters call it.
func (a *PaymentAttempt) RestoreBoleto(in BoletoInput, barcode string) {
	a.boleto, a.barcode = in, barcode
}

func (a *PaymentAttempt) ID() AttemptID { return a.id }

func (a *PaymentAttempt) Payment() PaymentID { return a.payment }

func (a *PaymentAttempt) Provider() ProviderName { return a.provider }

func (a *PaymentAttempt) Status() AttemptStatus { return a.status }

// ExternalID is the provider's ID for the attempt, empty until the provider names it.
func (a *PaymentAttempt) ExternalID() string { return a.externalID }

func (a *PaymentAttempt) BRCode() string { return a.brCode }

// Card is stored so a request that died before calling the provider can still be
// completed the same way.
func (a *PaymentAttempt) Card() CardInput { return a.card }

func (a *PaymentAttempt) Boleto() BoletoInput { return a.boleto }

// Barcode is the 44 digits; the linha digitável is derived from it.
func (a *PaymentAttempt) Barcode() string { return a.barcode }

func (a *PaymentAttempt) CreatedAt() time.Time { return a.createdAt }

func (a *PaymentAttempt) UpdatedAt() time.Time { return a.updatedAt }

// AttemptUpdate leaves empty what the answer does not carry.
type AttemptUpdate struct {
	Event      AttemptEvent
	ExternalID string
	BRCode     string
	Barcode    string
	// PaidAt and Amount come with Succeed.
	PaidAt time.Time
	Amount money.Money
}

// Apply refuses a value the attempt already has with another content: the provider
// cannot rename an attempt or change a code a payer may have scanned or printed. A
// duplicate that first reveals the external ID still counts as a change.
func (a *PaymentAttempt) Apply(u *AttemptUpdate, now time.Time) (changed bool, err error) {
	to, moved, err := TransitionAttempt(a.status, u.Event)
	if err != nil {
		return false, err
	}
	learnedID, err := learn(a.externalID, u.ExternalID, ErrExternalIDMismatch)
	if err != nil {
		return false, fmt.Errorf("attempt %s: %w", a.id, err)
	}
	learnedCode, err := learn(a.brCode, u.BRCode, ErrBRCodeMismatch)
	if err != nil {
		return false, fmt.Errorf("attempt %s: %w", a.id, err)
	}
	learnedBarcode, err := learn(a.barcode, u.Barcode, ErrBarcodeMismatch)
	if err != nil {
		return false, fmt.Errorf("attempt %s: %w", a.id, err)
	}
	if !moved && !learnedID && !learnedCode && !learnedBarcode {
		return false, nil
	}
	a.status = to
	if learnedID {
		a.externalID = u.ExternalID
	}
	if learnedCode {
		a.brCode = u.BRCode
	}
	if learnedBarcode {
		a.barcode = u.Barcode
	}
	a.updatedAt = now
	return true, nil
}

func learn(have, got string, mismatch error) (bool, error) {
	switch {
	case got == "" || got == have:
		return false, nil
	case have == "":
		return true, nil
	default:
		return false, fmt.Errorf("%w: has %q, got %q", mismatch, have, got)
	}
}
