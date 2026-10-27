package domain

import (
	"errors"
	"fmt"
)

var ErrUnknownAttemptStatus = errors.New("payments: unknown attempt status")

type AttemptStatus string

const (
	AttemptCreated   AttemptStatus = "created"
	AttemptSent      AttemptStatus = "sent"
	AttemptUnknown   AttemptStatus = "unknown"
	AttemptAccepted  AttemptStatus = "accepted"
	AttemptRejected  AttemptStatus = "rejected"
	AttemptSucceeded AttemptStatus = "succeeded"
)

func AttemptStatuses() []AttemptStatus {
	return []AttemptStatus{AttemptCreated, AttemptSent, AttemptUnknown, AttemptAccepted, AttemptRejected, AttemptSucceeded}
}

func ParseAttemptStatus(s string) (AttemptStatus, error) {
	for _, st := range AttemptStatuses() {
		if string(st) == s {
			return st, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownAttemptStatus, s)
}

type AttemptEvent int

const (
	Send AttemptEvent = iota + 1
	Accept
	Reject
	// Ambiguous is a timeout or an error that does not say whether the provider acted.
	Ambiguous
	Succeed
)

func AttemptEvents() []AttemptEvent {
	return []AttemptEvent{Send, Accept, Reject, Ambiguous, Succeed}
}

func (e AttemptEvent) String() string {
	switch e {
	case Send:
		return "send"
	case Accept:
		return "accept"
	case Reject:
		return "reject"
	case Ambiguous:
		return "ambiguous"
	case Succeed:
		return "succeed"
	default:
		return fmt.Sprintf("AttemptEvent(%d)", int(e))
	}
}

type attemptEdge struct {
	from  AttemptStatus
	event AttemptEvent
}

// attemptTransitions is the state machine of section 3.5. Succeed leaves sent and
// unknown too: providers reorder, so the payment event can beat the create response.
var attemptTransitions = map[attemptEdge]AttemptStatus{
	{AttemptCreated, Send}: AttemptSent,

	{AttemptSent, Accept}:    AttemptAccepted,
	{AttemptSent, Reject}:    AttemptRejected,
	{AttemptSent, Ambiguous}: AttemptUnknown,
	{AttemptSent, Succeed}:   AttemptSucceeded,

	{AttemptUnknown, Accept}:  AttemptAccepted,
	{AttemptUnknown, Reject}:  AttemptRejected,
	{AttemptUnknown, Succeed}: AttemptSucceeded,

	{AttemptAccepted, Succeed}: AttemptSucceeded,
}

// attemptReflected lists duplicates: a retried send, a lookup that times out again, a
// create response arriving after the payment event.
var attemptReflected = map[attemptEdge]bool{
	{AttemptSent, Send}:         true,
	{AttemptUnknown, Ambiguous}: true,
	{AttemptAccepted, Accept}:   true,
	{AttemptRejected, Reject}:   true,
	{AttemptSucceeded, Accept}:  true,
	{AttemptSucceeded, Succeed}: true,
}

// TransitionAttempt returns the status after the event. changed is false for a
// duplicate the status already reflects.
func TransitionAttempt(from AttemptStatus, e AttemptEvent) (to AttemptStatus, changed bool, err error) {
	if next, ok := attemptTransitions[attemptEdge{from, e}]; ok {
		return next, true, nil
	}
	if from == AttemptRejected && e == Succeed {
		return from, false, fmt.Errorf("%w: %w", ErrInvalidTransition, ErrPaidAfterRejection)
	}
	if attemptReflected[attemptEdge{from, e}] {
		return from, false, nil
	}
	return from, false, fmt.Errorf("%w: attempt %s on %s", ErrInvalidTransition, e, from)
}
