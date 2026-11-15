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

// Event's ID is unique per fact, so the receiver can drop a redelivery.
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

func Sign(secret []byte, t time.Time, body []byte) string { return simkit.Sign(secret, t, body) }

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
