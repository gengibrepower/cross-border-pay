// Package domain holds the rules of delivering webhooks to merchants (section 4.5):
// what counts as delivered, when a failed delivery is tried again (decision 23) and
// how a delivery is signed.
package domain

import (
	"fmt"
	"time"
)

// Decision 23: full-jitter exponential backoff from RetryBase, doubling up to RetryCap
// per wait, until GiveUpAfter since the event was created: about 20 attempts.
const (
	RetryBase   = 30 * time.Second
	RetryCap    = 6 * time.Hour
	GiveUpAfter = 72 * time.Hour
)

// DeliveredRetention is how long a delivered event is kept, for the merchant to look
// back at, before the cleanup deletes it (section 4.5).
const DeliveredRetention = 30 * 24 * time.Hour

// Answer is how one attempt ended: the status the merchant answered with, or Err when
// no answer came (refused address, timeout, connection error). StatusCode is 0 then.
type Answer struct {
	StatusCode int
	Err        error
}

// Succeeded is a 2xx and nothing else: a redirect is not followed, since it could
// point inside the network, and counts as a failure like any other status.
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

// Decide is where an event goes after an attempt at now that ended in a.
func Decide(a Answer, createdAt, now time.Time) Next {
	switch {
	case a.Succeeded():
		return Delivered
	case now.Sub(createdAt) >= GiveUpAfter:
		return GiveUp
	default:
		return Retry
	}
}

// RetryCeiling bounds the wait after the attempt-th one: RetryBase · 2^(attempt-1), at
// most RetryCap. Full jitter draws the actual wait uniformly from [0, ceiling], so the
// events of a merchant that was down and came back do not all return at once.
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
