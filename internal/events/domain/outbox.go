package domain

import (
	"errors"
	"time"
)

var ErrOutgoingNotFound = errors.New("events: outgoing event not found")

// Outgoing's Payload is what every delivery sends and signs, as its module serialized
// it (section 4.5).
type Outgoing struct {
	ID        [16]byte
	Merchant  [16]byte
	Type      string
	Subject   string
	Payload   []byte
	CreatedAt time.Time
}

// Delivery's Attempt fences what is recorded: a claim whose lease ran out and was
// taken over can no longer reschedule or fail the event. A window opens at the event's
// creation or its last manual redelivery.
type Delivery struct {
	Event              Outgoing
	Attempt            int
	WindowStart        time.Time
	WindowFirstAttempt int
}

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

// Listed's LastStatusCode is 0 when the last attempt got no answer, or none was made.
type Listed struct {
	Event          Outgoing
	Status         DeliveryStatus
	Attempts       int
	NextAttemptAt  time.Time
	LastStatusCode int
	LastError      string
	FinishedAt     time.Time
}

type ListQuery struct {
	Merchant [16]byte
	Status   DeliveryStatus
	Before   [16]byte
	Limit    int
}
