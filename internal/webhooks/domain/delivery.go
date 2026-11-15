// Package domain is section 4.5: what counts as delivered, the retries of decision 23
// and the signature.
package domain

import (
	"fmt"
	"time"
)

// Decision 23: full-jitter exponential backoff from RetryBase, doubling up to RetryCap
// per wait, until GiveUpAfter since the retry window opened, at the event's creation or
// at its last manual redelivery: about 20 attempts.
const (
	RetryBase   = 30 * time.Second
	RetryCap    = 6 * time.Hour
	GiveUpAfter = 72 * time.Hour
)

const DeliveredRetention = 30 * 24 * time.Hour

// Answer's StatusCode is 0 when no answer came.
type Answer struct {
	StatusCode int
	Err        error
}

// Succeeded is a 2xx only: a redirect is not followed, since it could point inside the
// network.
func (a Answer) Succeeded() bool {
	return a.Err == nil && a.StatusCode >= 200 && a.StatusCode < 300
}

func (a Answer) Reason() string {
	switch {
	case a.Err != nil:
		return a.Err.Error()
	case a.Succeeded():
		return ""
	default:
		return fmt.Sprintf("answered %d", a.StatusCode)
	}
}

type Next int

const (
	Delivered Next = iota + 1
	Retry
	GiveUp
)

func Decide(a Answer, windowStart, now time.Time) Next {
	switch {
	case a.Succeeded():
		return Delivered
	case now.Sub(windowStart) >= GiveUpAfter:
		return GiveUp
	default:
		return Retry
	}
}

// RetryCeiling: full jitter draws the wait from [0, ceiling], so the events of a
// merchant that was down and came back do not all return at once.
func RetryCeiling(attempt int) time.Duration {
	d := RetryBase
	for i := 1; i < attempt; i++ {
		if d >= RetryCap/2 {
			return RetryCap
		}
		d *= 2
	}
	return d
}
