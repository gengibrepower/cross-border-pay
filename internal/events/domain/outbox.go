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
