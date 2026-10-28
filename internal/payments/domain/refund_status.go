package domain

import (
	"errors"
	"fmt"
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
