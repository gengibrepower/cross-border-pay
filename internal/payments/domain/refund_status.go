package domain

import (
	"errors"
	"fmt"
	"time"
)

var ErrUnknownRefundStatus = errors.New("payments: unknown refund status")

type RefundStatus string

const (
	RefundCreated   RefundStatus = "created"
	RefundSent      RefundStatus = "sent"
	RefundUnknown   RefundStatus = "unknown"
	RefundSucceeded RefundStatus = "succeeded"
	RefundRejected  RefundStatus = "rejected"
)

func RefundStatuses() []RefundStatus {
	return []RefundStatus{RefundCreated, RefundSent, RefundUnknown, RefundSucceeded, RefundRejected}
}

func ParseRefundStatus(s string) (RefundStatus, error) {
	for _, st := range RefundStatuses() {
		if string(st) == s {
			return st, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownRefundStatus, s)
}

type RefundEvent int

const (
	SendRefund RefundEvent = iota + 1
	// RefundAmbiguous is a timeout or an error that does not say whether the money went.
	RefundAmbiguous
	SucceedRefund
	RejectRefund
)

func RefundEvents() []RefundEvent {
	return []RefundEvent{SendRefund, RefundAmbiguous, SucceedRefund, RejectRefund}
}

func (e RefundEvent) String() string {
	switch e {
	case SendRefund:
		return "send"
	case RefundAmbiguous:
		return "ambiguous"
	case SucceedRefund:
		return "succeed"
	case RejectRefund:
		return "reject"
	default:
		return fmt.Sprintf("RefundEvent(%d)", int(e))
	}
}

type refundEdge struct {
	from  RefundStatus
	event RefundEvent
}

// refundTransitions is section 3.5 for refunds: the provider's answer, or the lookup
// of decision 17 after an ambiguous one, ends the refund.
var refundTransitions = map[refundEdge]RefundStatus{
	{RefundCreated, SendRefund}: RefundSent,

	{RefundSent, SucceedRefund}:   RefundSucceeded,
	{RefundSent, RejectRefund}:    RefundRejected,
	{RefundSent, RefundAmbiguous}: RefundUnknown,

	{RefundUnknown, SucceedRefund}: RefundSucceeded,
	{RefundUnknown, RejectRefund}:  RefundRejected,
}

var refundReflected = map[refundEdge]bool{
	{RefundSent, SendRefund}:         true,
	{RefundUnknown, RefundAmbiguous}: true,
	{RefundSucceeded, SucceedRefund}: true,
	{RefundRejected, RejectRefund}:   true,
}

// TransitionRefund returns the status after the event. changed is false for a
// duplicate the status already reflects. A provider that contradicts a final answer,
// succeeded then rejected or the reverse, is ErrInvalidTransition: that needs a person.
func TransitionRefund(from RefundStatus, e RefundEvent) (to RefundStatus, changed bool, err error) {
	if next, ok := refundTransitions[refundEdge{from, e}]; ok {
		return next, true, nil
	}
	if refundReflected[refundEdge{from, e}] {
		return from, false, nil
	}
	return from, false, fmt.Errorf("%w: refund %s on %s", ErrInvalidTransition, e, from)
}

// Refund is one refund of a payment at its provider. Its status only changes through
// Apply.
type Refund struct {
	id         RefundID
	payment    PaymentID
	provider   ProviderName
	status     RefundStatus
	amounts    RefundAmounts
	externalID string
	reason     string
	createdAt  time.Time
	updatedAt  time.Time
}

// NewRefund is a refund priced and about to be stored, before the provider is called.
func NewRefund(id RefundID, payment PaymentID, provider ProviderName, amounts *RefundAmounts, now time.Time) Refund {
	return Refund{id: id, payment: payment, provider: provider, status: RefundCreated, amounts: *amounts, createdAt: now, updatedAt: now}
}

// RestoreRefund rebuilds a refund from storage; only adapters call it.
func RestoreRefund(id RefundID, payment PaymentID, provider ProviderName, status RefundStatus, amounts *RefundAmounts,
	externalID, reason string, createdAt, updatedAt time.Time,
) Refund {
	return Refund{
		id: id, payment: payment, provider: provider, status: status, amounts: *amounts,
		externalID: externalID, reason: reason, createdAt: createdAt, updatedAt: updatedAt,
	}
}

func (r *Refund) ID() RefundID { return r.id }

func (r *Refund) Payment() PaymentID { return r.payment }

func (r *Refund) Provider() ProviderName { return r.provider }

func (r *Refund) Status() RefundStatus { return r.status }

func (r *Refund) Amounts() RefundAmounts { return r.amounts }

// ExternalID is the provider's ID for the refund, empty until the provider names it.
func (r *Refund) ExternalID() string { return r.externalID }

// Reason is the provider's words for a rejection.
func (r *Refund) Reason() string { return r.reason }

func (r *Refund) CreatedAt() time.Time { return r.createdAt }

func (r *Refund) UpdatedAt() time.Time { return r.updatedAt }

// RefundUpdate is what one answer says about a refund.
type RefundUpdate struct {
	Event      RefundEvent
	ExternalID string
	Reason     string
}

// Apply moves the refund by the update's event and records the provider's ID the first
// time it is named; a provider renaming a refund is refused.
func (r *Refund) Apply(u *RefundUpdate, now time.Time) (changed bool, err error) {
	to, moved, err := TransitionRefund(r.status, u.Event)
	if err != nil {
		return false, err
	}
	learnedID, err := learn(r.externalID, u.ExternalID, ErrExternalIDMismatch)
	if err != nil {
		return false, fmt.Errorf("refund %s: %w", r.id, err)
	}
	if !moved && !learnedID {
		return false, nil
	}
	r.status = to
	if learnedID {
		r.externalID = u.ExternalID
	}
	if moved && u.Reason != "" {
		r.reason = u.Reason
	}
	r.updatedAt = now
	return true, nil
}
