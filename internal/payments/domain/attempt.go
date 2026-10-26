package domain

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidAttemptID = errors.New("payments: invalid attempt ID")
	// ErrExternalIDMismatch is a provider naming one attempt with two different IDs.
	ErrExternalIDMismatch = errors.New("payments: provider changed the attempt's external ID")
	ErrBRCodeMismatch     = errors.New("payments: provider changed the attempt's BR Code")
)

// AttemptID is also the idempotency key sent to the provider, so an attempt in unknown
// can be looked up before the provider's own ID is known.
type AttemptID [16]byte

func (id AttemptID) String() string { return "att_" + idEncoding.EncodeToString(id[:]) }

func ParseAttemptID(s string) (AttemptID, error) {
	id, ok := parseID("att_", s)
	if !ok {
		return AttemptID{}, fmt.Errorf("%w: %q", ErrInvalidAttemptID, s)
	}
	return id, nil
}

// ProviderName names the adapter an attempt was made with, such as "pixsim".
type ProviderName string

// PaymentAttempt is one try at collecting a payment through a provider (decision 6).
// Its status only changes through Apply.
type PaymentAttempt struct {
	id         AttemptID
	payment    PaymentID
	provider   ProviderName
	status     AttemptStatus
	externalID string
	brCode     string
	createdAt  time.Time
	updatedAt  time.Time
}

// RestoreAttempt rebuilds an attempt from storage; only adapters call it.
func RestoreAttempt(id AttemptID, payment PaymentID, provider ProviderName, status AttemptStatus, externalID, brCode string, createdAt, updatedAt time.Time) PaymentAttempt {
	return PaymentAttempt{
		id: id, payment: payment, provider: provider, status: status,
		externalID: externalID, brCode: brCode, createdAt: createdAt, updatedAt: updatedAt,
	}
}

func (a *PaymentAttempt) ID() AttemptID { return a.id }

func (a *PaymentAttempt) Payment() PaymentID { return a.payment }

func (a *PaymentAttempt) Provider() ProviderName { return a.provider }

func (a *PaymentAttempt) Status() AttemptStatus { return a.status }

// ExternalID is the provider's ID for the attempt, empty until the provider names it.
func (a *PaymentAttempt) ExternalID() string { return a.externalID }

// BRCode is the PIX code the payer pays, empty until the provider returns it.
func (a *PaymentAttempt) BRCode() string { return a.brCode }

func (a *PaymentAttempt) CreatedAt() time.Time { return a.createdAt }

func (a *PaymentAttempt) UpdatedAt() time.Time { return a.updatedAt }

// AttemptUpdate is what one answer or event says about an attempt. ExternalID and
// BRCode are empty when it does not carry them.
type AttemptUpdate struct {
	Event      AttemptEvent
	ExternalID string
	BRCode     string
}

// Apply moves the attempt by the update's event and records what the provider named
// for the first time. A value the attempt already has with another content is refused:
// the provider cannot rename an attempt or change the code a payer may have scanned.
// changed is false when there is nothing new to store, so a duplicate that first
// reveals the external ID still counts as a change.
func (a *PaymentAttempt) Apply(u AttemptUpdate, now time.Time) (changed bool, err error) {
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
	if !moved && !learnedID && !learnedCode {
		return false, nil
	}
	a.status = to
	if learnedID {
		a.externalID = u.ExternalID
	}
	if learnedCode {
		a.brCode = u.BRCode
	}
	a.updatedAt = now
	return true, nil
}

// learn reports whether got is new for a field that holds have.
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
