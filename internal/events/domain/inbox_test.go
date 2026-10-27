package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/events/domain"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func TestRetryBackoff(t *testing.T) {
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second,
		5 * time.Minute, 5 * time.Minute,
	}
	for attempts, w := range want {
		if got := domain.RetryBackoff(attempts); got != w {
			t.Errorf("RetryBackoff(%d) = %v, want %v", attempts, got, w)
		}
	}
	if got := domain.RetryBackoff(1 << 20); got != domain.RetryCap {
		t.Errorf("RetryBackoff(huge) = %v, want the cap %v", got, domain.RetryCap)
	}
}

func TestNext(t *testing.T) {
	e := domain.Event{Attempts: 2, ReceivedAt: t0}
	now := t0.Add(time.Hour)
	for _, tc := range []struct {
		outcome domain.Outcome
		status  domain.Status
		retryAt time.Time
	}{
		{domain.Processed, domain.StatusProcessed, time.Time{}},
		{domain.Ignored, domain.StatusIgnored, time.Time{}},
		{domain.Failed, domain.StatusFailed, time.Time{}},
		{domain.Retry, domain.StatusPending, now.Add(20 * time.Second)},
	} {
		status, retryAt, err := e.Next(tc.outcome, now)
		if err != nil || status != tc.status || !retryAt.Equal(tc.retryAt) {
			t.Errorf("Next(%d) = %s, %v, %v; want %s, %v", tc.outcome, status, retryAt, err, tc.status, tc.retryAt)
		}
	}
	if _, _, err := e.Next(0, now); !errors.Is(err, domain.ErrUnknownOutcome) {
		t.Errorf("Next(zero outcome) error = %v, want %v", err, domain.ErrUnknownOutcome)
	}
}

// Decision 52: a dependency that has not arrived in 24 h is not coming; a person looks.
func TestRetryGivesUpAfterADay(t *testing.T) {
	e := domain.Event{ReceivedAt: t0}
	if status, _, _ := e.Next(domain.Retry, t0.Add(domain.GiveUpAfter-time.Second)); status != domain.StatusPending {
		t.Errorf("just before the limit: %s, want pending", status)
	}
	if status, _, _ := e.Next(domain.Retry, t0.Add(domain.GiveUpAfter)); status != domain.StatusFailed {
		t.Errorf("at the limit: %s, want failed", status)
	}
}

func TestIncomingValidate(t *testing.T) {
	ok := domain.Incoming{Provider: "pixsim", ExternalID: "evt_1", Type: "charge.paid"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	for name, in := range map[string]domain.Incoming{
		"no provider":    {ExternalID: "evt_1", Type: "charge.paid"},
		"long provider":  {Provider: strings.Repeat("p", 51), ExternalID: "evt_1", Type: "charge.paid"},
		"no external ID": {Provider: "pixsim", Type: "charge.paid"},
		"long external":  {Provider: "pixsim", ExternalID: strings.Repeat("e", 101), Type: "charge.paid"},
		"no type":        {Provider: "pixsim", ExternalID: "evt_1"},
		"long type":      {Provider: "pixsim", ExternalID: "evt_1", Type: strings.Repeat("t", 101)},
	} {
		if err := in.Validate(); !errors.Is(err, domain.ErrInvalidEvent) {
			t.Errorf("%s: Validate() = %v, want %v", name, err, domain.ErrInvalidEvent)
		}
	}
}
