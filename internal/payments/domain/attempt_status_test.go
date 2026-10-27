package domain_test

import (
	"errors"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

type attemptWant struct {
	outcome outcome
	to      domain.AttemptStatus
}

// expectedAttempt is section 3.5 written out again, independently of the production
// table: every (status, event) pair not listed here must be ErrInvalidTransition.
var expectedAttempt = map[domain.AttemptStatus]map[domain.AttemptEvent]attemptWant{
	domain.AttemptCreated: {
		domain.Send: {moves, domain.AttemptSent},
	},
	domain.AttemptSent: {
		domain.Send:      {duplicate, domain.AttemptSent},
		domain.Accept:    {moves, domain.AttemptAccepted},
		domain.Reject:    {moves, domain.AttemptRejected},
		domain.Ambiguous: {moves, domain.AttemptUnknown},
		domain.Succeed:   {moves, domain.AttemptSucceeded},
	},
	domain.AttemptUnknown: {
		domain.Accept:    {moves, domain.AttemptAccepted},
		domain.Reject:    {moves, domain.AttemptRejected},
		domain.Ambiguous: {duplicate, domain.AttemptUnknown},
		domain.Succeed:   {moves, domain.AttemptSucceeded},
	},
	domain.AttemptAccepted: {
		domain.Accept:  {duplicate, domain.AttemptAccepted},
		domain.Succeed: {moves, domain.AttemptSucceeded},
	},
	domain.AttemptRejected: {
		domain.Reject: {duplicate, domain.AttemptRejected},
		// Money for a rejected attempt is a divergence (section 4.4), not a move.
		domain.Succeed: {late, domain.AttemptRejected},
	},
	domain.AttemptSucceeded: {
		domain.Accept:  {duplicate, domain.AttemptSucceeded},
		domain.Succeed: {duplicate, domain.AttemptSucceeded},
	},
}

func TestTransitionAttemptIsExhaustive(t *testing.T) {
	if len(expectedAttempt) != len(domain.AttemptStatuses()) {
		t.Fatalf("expected covers %d statuses, domain has %d", len(expectedAttempt), len(domain.AttemptStatuses()))
	}
	for _, from := range domain.AttemptStatuses() {
		for _, e := range domain.AttemptEvents() {
			w := expectedAttempt[from][e]
			to, changed, err := domain.TransitionAttempt(from, e)

			switch w.outcome {
			case moves:
				if err != nil || !changed || to != w.to {
					t.Errorf("%s on %s = (%s, %v, %v), want move to %s", e, from, to, changed, err, w.to)
				}
			case duplicate:
				if err != nil || changed || to != from {
					t.Errorf("%s on %s = (%s, %v, %v), want no-op", e, from, to, changed, err)
				}
			case invalid, late:
				if !errors.Is(err, domain.ErrInvalidTransition) || changed || to != from {
					t.Errorf("%s on %s = (%s, %v, %v), want ErrInvalidTransition", e, from, to, changed, err)
				}
				if errors.Is(err, domain.ErrPaidAfterRejection) != (w.outcome == late) {
					t.Errorf("%s on %s: error = %v, paid after rejection only for money on a rejected attempt", e, from, err)
				}
			}
		}
	}
}

func TestTransitionAttemptRejectsUnknownStatusAndEvent(t *testing.T) {
	if _, _, err := domain.TransitionAttempt("pending", domain.Send); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("unknown status: error = %v, want %v", err, domain.ErrInvalidTransition)
	}
	if _, _, err := domain.TransitionAttempt(domain.AttemptSent, domain.AttemptEvent(0)); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("zero event: error = %v, want %v", err, domain.ErrInvalidTransition)
	}
}

func TestTerminalAttemptStatusesOnlyAcceptDuplicates(t *testing.T) {
	for _, from := range []domain.AttemptStatus{domain.AttemptRejected, domain.AttemptSucceeded} {
		for _, e := range domain.AttemptEvents() {
			if _, changed, _ := domain.TransitionAttempt(from, e); changed {
				t.Errorf("%s left terminal status %s", e, from)
			}
		}
	}
}

// Section 4.3: an ambiguous answer is never a failure. Only the provider saying no can
// end an attempt as rejected, and nothing returns an attempt to created.
func TestAmbiguousNeverRejects(t *testing.T) {
	for _, from := range domain.AttemptStatuses() {
		if to, _, err := domain.TransitionAttempt(from, domain.Ambiguous); err == nil && to == domain.AttemptRejected {
			t.Errorf("ambiguous on %s rejected the attempt", from)
		}
		for _, e := range domain.AttemptEvents() {
			if to, changed, _ := domain.TransitionAttempt(from, e); changed && to == domain.AttemptCreated {
				t.Errorf("%s on %s went back to created", e, from)
			}
		}
	}
}

func TestParseAttemptStatus(t *testing.T) {
	for _, s := range domain.AttemptStatuses() {
		got, err := domain.ParseAttemptStatus(string(s))
		if err != nil || got != s {
			t.Errorf("ParseAttemptStatus(%q) = %q, %v", s, got, err)
		}
	}
	if _, err := domain.ParseAttemptStatus("pending"); !errors.Is(err, domain.ErrUnknownAttemptStatus) {
		t.Errorf("ParseAttemptStatus(pending) error = %v, want %v", err, domain.ErrUnknownAttemptStatus)
	}
}
