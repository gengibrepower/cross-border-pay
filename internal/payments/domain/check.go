package domain

import "time"

// Decision 17 with the numbers of decision 52: an ambiguous attempt is looked up with
// exponential backoff, and handed to a person after ReviewAfter.
const (
	CheckBase   = 5 * time.Second
	CheckCap    = 5 * time.Minute
	ReviewAfter = 24 * time.Hour
	// SentStaleAfter is far above the provider timeout, so only a request that died
	// stays sent that long.
	SentStaleAfter = time.Minute
	// NotFoundGrace: until then, a provider that does not know the attempt may still
	// be about to receive it.
	NotFoundGrace = time.Minute
)

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

// FirstRefundCheck queues a created refund too: its row and hold commit before the
// provider is called, so one still created later was left by a request that died.
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
