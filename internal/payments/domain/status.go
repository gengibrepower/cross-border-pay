package domain

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidTransition = errors.New("payments: invalid transition")
	// ErrLatePayment is a confirmation for a payment that can no longer be paid. The
	// money still arrived, so the caller must not drop it (decision 8).
	ErrLatePayment   = errors.New("payments: confirmation after the payment closed")
	ErrUnknownStatus = errors.New("payments: unknown status")
)

type Status string

const (
	Pending           Status = "pending"
	Paid              Status = "paid"
	PartiallyRefunded Status = "partially_refunded"
	Refunded          Status = "refunded"
	ChargedBack       Status = "charged_back"
	Expired           Status = "expired"
	Canceled          Status = "canceled"
)

func Statuses() []Status {
	return []Status{Pending, Paid, PartiallyRefunded, Refunded, ChargedBack, Expired, Canceled}
}

func ParseStatus(s string) (Status, error) {
	for _, st := range Statuses() {
		if string(st) == s {
			return st, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownStatus, s)
}

type Event int

const (
	Confirm Event = iota + 1
	Expire
	Cancel
	RefundPartial
	RefundFull
	Chargeback
)

func Events() []Event {
	return []Event{Confirm, Expire, Cancel, RefundPartial, RefundFull, Chargeback}
}

func (e Event) String() string {
	switch e {
	case Confirm:
		return "confirm"
	case Expire:
		return "expire"
	case Cancel:
		return "cancel"
	case RefundPartial:
		return "refund_partial"
	case RefundFull:
		return "refund_full"
	case Chargeback:
		return "chargeback"
	default:
		return fmt.Sprintf("Event(%d)", int(e))
	}
}

type edge struct {
	from  Status
	event Event
}

// transitions is the state machine of section 3.4, the only place a status changes.
var transitions = map[edge]Status{
	{Pending, Confirm}: Paid,
	{Pending, Expire}:  Expired,
	{Pending, Cancel}:  Canceled,

	{Paid, RefundPartial}: PartiallyRefunded,
	{Paid, RefundFull}:    Refunded,
	{Paid, Chargeback}:    ChargedBack,

	{PartiallyRefunded, RefundPartial}: PartiallyRefunded,
	{PartiallyRefunded, RefundFull}:    Refunded,
	{PartiallyRefunded, Chargeback}:    ChargedBack,

	// Decision 7: a chargeback can still arrive after a full refund.
	{Refunded, Chargeback}: ChargedBack,
}

// reflected lists events a status already reflects: seeing them again is a duplicate
// delivery, not an error (section 3.5). A repeated partial refund is not in this list
// because each partial refund is new money moving.
var reflected = map[edge]bool{
	{Paid, Confirm}:              true,
	{PartiallyRefunded, Confirm}: true,
	{Refunded, Confirm}:          true,
	{ChargedBack, Confirm}:       true,
	{Expired, Expire}:            true,
	{Canceled, Cancel}:           true,
	{Refunded, RefundFull}:       true,
	{ChargedBack, Chargeback}:    true,
}

// Transition returns the status after the event. changed is false for a duplicate the
// status already reflects.
func Transition(from Status, e Event) (to Status, changed bool, err error) {
	if next, ok := transitions[edge{from, e}]; ok {
		return next, true, nil
	}
	if reflected[edge{from, e}] {
		return from, false, nil
	}
	if e == Confirm && (from == Expired || from == Canceled) {
		return from, false, fmt.Errorf("%w: %w: payment is %s", ErrInvalidTransition, ErrLatePayment, from)
	}
	return from, false, fmt.Errorf("%w: %s on %s", ErrInvalidTransition, e, from)
}
