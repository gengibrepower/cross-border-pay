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
	createdAt  time.Time
	updatedAt  time.Time
}

// RestoreAttempt rebuilds an attempt from storage; only adapters call it.
func RestoreAttempt(id AttemptID, payment PaymentID, provider ProviderName, status AttemptStatus, externalID string, createdAt, updatedAt time.Time) PaymentAttempt {
	return PaymentAttempt{
		id: id, payment: payment, provider: provider, status: status,
		externalID: externalID, createdAt: createdAt, updatedAt: updatedAt,
	}
}

func (a *PaymentAttempt) ID() AttemptID { return a.id }

func (a *PaymentAttempt) Payment() PaymentID { return a.payment }

func (a *PaymentAttempt) Provider() ProviderName { return a.provider }

func (a *PaymentAttempt) Status() AttemptStatus { return a.status }

// ExternalID is the provider's ID for the attempt, empty until the provider names it.
func (a *PaymentAttempt) ExternalID() string { return a.externalID }

func (a *PaymentAttempt) CreatedAt() time.Time { return a.createdAt }

func (a *PaymentAttempt) UpdatedAt() time.Time { return a.updatedAt }

// Apply moves the attempt by the event and records the provider's ID when the event
// brings one. changed is false when there is nothing new to store, so a duplicate that
// first reveals the external ID still counts as a change.
func (a *PaymentAttempt) Apply(e AttemptEvent, externalID string, now time.Time) (changed bool, err error) {
	to, moved, err := TransitionAttempt(a.status, e)
	if err != nil {
		return false, err
	}
	learned := false
	if externalID != "" {
		switch a.externalID {
		case "":
			learned = true
		case externalID:
		default:
			return false, fmt.Errorf("%w: attempt %s is %q, got %q", ErrExternalIDMismatch, a.id, a.externalID, externalID)
		}
	}
	if !moved && !learned {
		return false, nil
	}
	a.status = to
	if learned {
		a.externalID = externalID
	}
	a.updatedAt = now
	return true, nil
}
