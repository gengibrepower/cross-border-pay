package pixsim

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
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
	SignatureHeader = "Pixsim-Signature"
	// SignatureTolerance bounds how old (or how far ahead) a signature may be, so a
	// captured request cannot be replayed later.
	SignatureTolerance = 5 * time.Minute
)

var (
	ErrBadSignature   = errors.New("pixsim: event signature does not match")
	ErrStaleSignature = errors.New("pixsim: event signature is too old or from the future")
	ErrBadEvent       = errors.New("pixsim: malformed event")
)

// Sign returns the signature header for body sent at t: the HMAC-SHA256 of the Unix
// time, a dot and the body. Signing the time with the body is what makes the
// tolerance check mean anything.
func Sign(secret []byte, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac(secret, ts, body))
}

func mac(secret []byte, ts string, body []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(ts))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}

// VerifyEvent checks the signature header against body and parses the event. Any v1
// may match, so the sender can sign with an old and a new secret while rotating.
func VerifyEvent(secret []byte, header string, body []byte, now time.Time) (Event, error) {
	var ts string
	var sigs [][]byte
	for part := range strings.SplitSeq(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			if sig, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, sig)
			}
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return Event{}, fmt.Errorf("%w: header %q", ErrBadSignature, header)
	}
	if d := now.Sub(time.Unix(sec, 0)); d > SignatureTolerance || d < -SignatureTolerance {
		return Event{}, fmt.Errorf("%w: signed %v away from now", ErrStaleSignature, d)
	}
	want := mac(secret, ts, body)
	matched := false
	for _, sig := range sigs {
		// Constant time: comparing byte by byte and stopping at the first difference
		// would tell an attacker, through timing, how much of a forgery is right.
		matched = hmac.Equal(sig, want) || matched
	}
	if !matched {
		return Event{}, ErrBadSignature
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
