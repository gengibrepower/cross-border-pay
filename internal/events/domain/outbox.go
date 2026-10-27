package domain

import (
	"errors"
	"time"
)

var ErrOutgoingNotFound = errors.New("events: outgoing event not found")

// Outgoing is an event for a merchant (section 4.5). Payload is the event exactly as
// the module that produced it serialized it, and what every delivery sends and signs.
type Outgoing struct {
	ID        [16]byte
	Merchant  [16]byte
	Type      string
	Subject   string
	Payload   []byte
	CreatedAt time.Time
}

// Delivery is one claimed attempt at sending an outgoing event. Attempt counts the
// claims so far, this one included, and fences what is recorded about it: a claim
// whose lease ran out and was taken over can no longer reschedule or fail the event.
// The retry window opened at WindowStart, with the event's creation or its last manual
// redelivery, and its first claim was WindowFirstAttempt.
type Delivery struct {
	Event              Outgoing
	Attempt            int
	WindowStart        time.Time
	WindowFirstAttempt int
}

// WindowAttempt numbers this attempt within its window, from 1.
func (d *Delivery) WindowAttempt() int { return d.Attempt - d.WindowFirstAttempt + 1 }

type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliveryDelivered DeliveryStatus = "delivered"
	DeliveryFailed    DeliveryStatus = "failed"
)

func ParseDeliveryStatus(s string) (DeliveryStatus, bool) {
	switch st := DeliveryStatus(s); st {
	case DeliveryPending, DeliveryDelivered, DeliveryFailed:
		return st, true
	default:
		return "", false
	}
}

// Listed is an outgoing event with where its delivery stands, as the merchant sees it.
// LastStatusCode is 0 when the last attempt got no answer, or none was made.
type Listed struct {
	Event          Outgoing
	Status         DeliveryStatus
	Attempts       int
	NextAttemptAt  time.Time
	LastStatusCode int
	LastError      string
	FinishedAt     time.Time
}

// ListQuery is one page of a merchant's events, newest first: Before is the last ID of
// the previous page, zero for the first; Status zero for any.
type ListQuery struct {
	Merchant [16]byte
	Status   DeliveryStatus
	Before   [16]byte
	Limit    int
}
