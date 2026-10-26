package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

var (
	attemptCreatedAt = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	later            = attemptCreatedAt.Add(time.Second)
)

func attemptIn(status domain.AttemptStatus, externalID string) domain.PaymentAttempt {
	return domain.RestoreAttempt(domain.AttemptID{1}, domain.PaymentID{2}, "pixsim", status, externalID, attemptCreatedAt, attemptCreatedAt)
}

func TestApplyMovesAndLearnsTheExternalID(t *testing.T) {
	a := attemptIn(domain.AttemptSent, "")
	changed, err := a.Apply(domain.Accept, "ext-1", later)
	if err != nil || !changed {
		t.Fatalf("Apply(accept) = %v, %v; want a change", changed, err)
	}
	if a.Status() != domain.AttemptAccepted || a.ExternalID() != "ext-1" || !a.UpdatedAt().Equal(later) {
		t.Errorf("attempt = %s, %q, %v; want accepted, ext-1, %v", a.Status(), a.ExternalID(), a.UpdatedAt(), later)
	}
}

func TestApplyMovesWithTheSameExternalID(t *testing.T) {
	a := attemptIn(domain.AttemptAccepted, "ext-1")
	changed, err := a.Apply(domain.Succeed, "ext-1", later)
	if err != nil || !changed || a.Status() != domain.AttemptSucceeded {
		t.Errorf("Apply(succeed, ext-1) = %v, %v, %s; want a move to succeeded", changed, err, a.Status())
	}
}

func TestApplyDuplicateChangesNothing(t *testing.T) {
	a := attemptIn(domain.AttemptAccepted, "ext-1")
	for _, ext := range []string{"", "ext-1"} {
		changed, err := a.Apply(domain.Accept, ext, later)
		if err != nil || changed {
			t.Errorf("Apply(accept, %q) = %v, %v; want a no-op", ext, changed, err)
		}
	}
	if !a.UpdatedAt().Equal(attemptCreatedAt) {
		t.Errorf("a no-op touched UpdatedAt: %v", a.UpdatedAt())
	}
}

// The payment event can arrive before the create response; the late response is a
// duplicate for the status but the first to name the attempt.
func TestApplyDuplicateThatRevealsTheExternalIDIsAChange(t *testing.T) {
	a := attemptIn(domain.AttemptSucceeded, "")
	changed, err := a.Apply(domain.Accept, "ext-1", later)
	if err != nil || !changed {
		t.Fatalf("Apply(accept) = %v, %v; want a change", changed, err)
	}
	if a.Status() != domain.AttemptSucceeded || a.ExternalID() != "ext-1" {
		t.Errorf("attempt = %s, %q; want succeeded, ext-1", a.Status(), a.ExternalID())
	}
}

func TestApplyRejectsAnotherExternalID(t *testing.T) {
	a := attemptIn(domain.AttemptAccepted, "ext-1")
	if _, err := a.Apply(domain.Succeed, "ext-2", later); !errors.Is(err, domain.ErrExternalIDMismatch) {
		t.Fatalf("Apply(succeed, ext-2) error = %v, want %v", err, domain.ErrExternalIDMismatch)
	}
	if a.Status() != domain.AttemptAccepted || a.ExternalID() != "ext-1" {
		t.Errorf("a refused event changed the attempt: %s, %q", a.Status(), a.ExternalID())
	}
}

func TestApplyInvalidTransitionKeepsTheAttempt(t *testing.T) {
	a := attemptIn(domain.AttemptRejected, "")
	if _, err := a.Apply(domain.Succeed, "ext-1", later); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("Apply(succeed) on rejected error = %v, want %v", err, domain.ErrInvalidTransition)
	}
	if a.Status() != domain.AttemptRejected || a.ExternalID() != "" {
		t.Errorf("a refused event changed the attempt: %s, %q", a.Status(), a.ExternalID())
	}
}

func TestAttemptIDRoundTrip(t *testing.T) {
	id := domain.AttemptID{0x01, 0x9a, 0x2b, 15: 0xff}
	s := id.String()
	if !strings.HasPrefix(s, "att_") || len(s) != len("att_")+26 {
		t.Fatalf("String() = %q, want att_ and 26 characters", s)
	}
	got, err := domain.ParseAttemptID(s)
	if err != nil || got != id {
		t.Errorf("ParseAttemptID(%q) = %v, %v; want %v", s, got, err, id)
	}
	if _, err := domain.ParseAttemptID(domain.PaymentID(id).String()); !errors.Is(err, domain.ErrInvalidAttemptID) {
		t.Errorf("ParseAttemptID(payment ID) error = %v, want %v", err, domain.ErrInvalidAttemptID)
	}
}
