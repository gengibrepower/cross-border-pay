package domain

import "time"

// Decision 17 with the numbers of decision 52: an ambiguous attempt is looked up at the
// provider with exponential backoff until it resolves, and handed to a person after
// ReviewAfter.
const (
	CheckBase   = 5 * time.Second
	CheckCap    = 5 * time.Minute
	ReviewAfter = 24 * time.Hour
	// SentStaleAfter is how long an attempt may stay sent before it is treated as
	// unknown: far above the provider timeout, so only a request that died is left.
	SentStaleAfter = time.Minute
	// NotFoundGrace is how long a provider that does not know the attempt may still
	// be about to receive it; after that, not found means it never will.
	NotFoundGrace = time.Minute
)

// CheckBackoff is the wait before the next lookup after failed lookups:
// CheckBase · 2^failed, at most CheckCap.
func CheckBackoff(failed int) time.Duration {
	d := CheckBase
	for range failed {
		if d >= CheckCap/2 {
			return CheckCap
		}
		d *= 2
	}
	return d
}

// FirstCheck is when an attempt entering status must first be checked: sent waits to
// go stale, unknown waits CheckBase. ok is false for statuses nobody needs to check.
func FirstCheck(status AttemptStatus, now time.Time) (at time.Time, ok bool) {
	switch status {
	case AttemptSent:
		return now.Add(SentStaleAfter), true
	case AttemptUnknown:
		return now.Add(CheckBackoff(0)), true
	case AttemptCreated, AttemptAccepted, AttemptRejected, AttemptSucceeded:
		return time.Time{}, false
	default:
		return time.Time{}, false
	}
}

// FirstRefundCheck is FirstCheck for refunds. A created refund is in the queue too: its
// row and hold commit before the provider is called, so one still created after
// SentStaleAfter was left by a request that died, and the resolver sends it.
func FirstRefundCheck(status RefundStatus, now time.Time) (at time.Time, ok bool) {
	switch status {
	case RefundCreated, RefundSent:
		return now.Add(SentStaleAfter), true
	case RefundUnknown:
		return now.Add(CheckBackoff(0)), true
	case RefundSucceeded, RefundRejected:
		return time.Time{}, false
	default:
		return time.Time{}, false
	}
}
