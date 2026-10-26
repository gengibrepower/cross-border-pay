package domain_test

import (
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

func TestCheckBackoff(t *testing.T) {
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second,
		5 * time.Minute, 5 * time.Minute,
	}
	for failed, w := range want {
		if got := domain.CheckBackoff(failed); got != w {
			t.Errorf("CheckBackoff(%d) = %v, want %v", failed, got, w)
		}
	}
	if got := domain.CheckBackoff(1 << 20); got != domain.CheckCap {
		t.Errorf("CheckBackoff(huge) = %v, want the cap %v", got, domain.CheckCap)
	}
}

func TestFirstCheck(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, status := range domain.AttemptStatuses() {
		at, ok := domain.FirstCheck(status, now)
		switch status {
		case domain.AttemptSent:
			if !ok || !at.Equal(now.Add(domain.SentStaleAfter)) {
				t.Errorf("FirstCheck(sent) = %v, %v; want %v", at, ok, now.Add(domain.SentStaleAfter))
			}
		case domain.AttemptUnknown:
			if !ok || !at.Equal(now.Add(domain.CheckBase)) {
				t.Errorf("FirstCheck(unknown) = %v, %v; want %v", at, ok, now.Add(domain.CheckBase))
			}
		case domain.AttemptCreated, domain.AttemptAccepted, domain.AttemptRejected, domain.AttemptSucceeded:
			if ok {
				t.Errorf("FirstCheck(%s) = %v, want no check", status, at)
			}
		}
	}
}
