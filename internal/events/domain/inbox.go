// Package domain holds the inbox's rules: what an event received from a provider is,
// how its processing ends and how long a not-yet-applicable one keeps being retried.
package domain

import (
	"errors"
	"time"
)

var (
	ErrInvalidEvent   = errors.New("events: invalid event")
	ErrUnknownOutcome = errors.New("events: unknown outcome")
)

// Decision 52's numbers for events: an event whose dependency has not arrived is
// retried with backoff, and after GiveUpAfter it fails for a person to look at.
const (
	RetryBase   = 5 * time.Second
	RetryCap    = 5 * time.Minute
	GiveUpAfter = 24 * time.Hour
)

// Incoming is a verified event as the provider sent it. Payload and Signature are the
// exact bytes and header, kept as the proof.
type Incoming struct {
	Provider   string
	ExternalID string
	Type       string
	Payload    []byte
	Signature  string
}

func (in *Incoming) Validate() error {
	switch {
	case in.Provider == "" || len(in.Provider) > 50:
		return errors.Join(ErrInvalidEvent, errors.New("provider must have 1 to 50 characters"))
	case in.ExternalID == "" || len(in.ExternalID) > 100:
		return errors.Join(ErrInvalidEvent, errors.New("external ID must have 1 to 100 characters"))
	case in.Type == "" || len(in.Type) > 100:
		return errors.Join(ErrInvalidEvent, errors.New("type must have 1 to 100 characters"))
	}
	return nil
}

// Event is a stored event being processed.
type Event struct {
	ID         [16]byte
	Provider   string
	ExternalID string
	Type       string
	Payload    []byte
	Attempts   int
	ReceivedAt time.Time
}

// Outcome is how a handler's attempt at an event ends.
type Outcome int

const (
	// Processed: the effect was applied, or had already been by another path.
	Processed Outcome = iota + 1
	// Ignored: an event type nobody acts on.
	Ignored
	// Retry: it cannot be applied yet, such as a payment for an attempt not stored.
	Retry
	// Failed: it can never be applied as sent; a person must look at it.
	Failed
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusProcessed Status = "processed"
	StatusIgnored   Status = "ignored"
	StatusFailed    Status = "failed"
)

// Next is where an event goes after an attempt that ended in o at now: a final status,
// or pending again at retryAt.
func (e *Event) Next(o Outcome, now time.Time) (status Status, retryAt time.Time, err error) {
	switch o {
	case Processed:
		return StatusProcessed, time.Time{}, nil
	case Ignored:
		return StatusIgnored, time.Time{}, nil
	case Failed:
		return StatusFailed, time.Time{}, nil
	case Retry:
		if now.Sub(e.ReceivedAt) >= GiveUpAfter {
			return StatusFailed, time.Time{}, nil
		}
		return StatusPending, now.Add(RetryBackoff(e.Attempts)), nil
	default:
		return "", time.Time{}, ErrUnknownOutcome
	}
}

// RetryBackoff is the wait after attempts earlier tries: RetryBase · 2^attempts, at
// most RetryCap.
func RetryBackoff(attempts int) time.Duration {
	d := RetryBase
	for range attempts {
		if d >= RetryCap/2 {
			return RetryCap
		}
		d *= 2
	}
	return d
}
