package domain

import "time"

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
type Delivery struct {
	Event   Outgoing
	Attempt int
}
