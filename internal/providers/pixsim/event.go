package pixsim

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

type EventType string

const ChargePaid EventType = "charge.paid"

// Event is what the simulator posts to the webhook URL. Its ID is unique per fact, so
// a redelivery repeats it and the receiver can drop the duplicate.
type Event struct {
	ID        string    `json:"id"`
	Type      EventType `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Charge    Charge    `json:"charge"`
}

const (
	SignatureHeader    = "Pixsim-Signature"
	SignatureTolerance = simkit.SignatureTolerance
)

var (
	ErrBadSignature   = simkit.ErrBadSignature
	ErrStaleSignature = simkit.ErrStaleSignature
	ErrBadEvent       = errors.New("pixsim: malformed event")
)

// Sign returns the signature header for body sent at t (simkit.Sign).
func Sign(secret []byte, t time.Time, body []byte) string { return simkit.Sign(secret, t, body) }

// VerifyEvent checks the signature header against body and parses the event.
func VerifyEvent(secret []byte, header string, body []byte, now time.Time) (Event, error) {
	if err := simkit.Verify(secret, header, body, now); err != nil {
		return Event{}, err
	}
	var e Event
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Event{}, fmt.Errorf("%w: %w", ErrBadEvent, err)
	}
	if e.ID == "" || e.Type == "" {
		return Event{}, fmt.Errorf("%w: id and type are required", ErrBadEvent)
	}
	return e, nil
}
