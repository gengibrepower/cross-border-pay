package domain

import (
	"errors"
	"testing"
	"time"
)

func TestRetryCeilingDoublesFromBaseUpToCap(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{10, 30 * time.Second << 9},
		{11, RetryCap},
		{100, RetryCap},
	}
	for _, tc := range tests {
		if got := RetryCeiling(tc.attempt); got != tc.want {
			t.Errorf("RetryCeiling(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

// Every wait at its ceiling is the fewest attempts before giving up: 22, decision 23's
// "about 20". Full jitter only shortens the waits, so it adds attempts.
func TestGiveUpAfterHoldsAtLeast22Attempts(t *testing.T) {
	var at time.Duration
	attempt := 1
	for ; at < GiveUpAfter; attempt++ {
		at += RetryCeiling(attempt)
	}
	if attempt != 22 {
		t.Errorf("attempt %d is the first at or after %v, want 22", attempt, GiveUpAfter)
	}
}

func TestDecide(t *testing.T) {
	created := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		a    Answer
		at   time.Duration
		want Next
	}{
		{"200", Answer{StatusCode: 200}, 0, Delivered},
		{"204", Answer{StatusCode: 204}, 0, Delivered},
		{"2xx after the deadline still counts", Answer{StatusCode: 200}, 100 * time.Hour, Delivered},
		{"199", Answer{StatusCode: 199}, 0, Retry},
		{"redirect", Answer{StatusCode: 302}, 0, Retry},
		{"300", Answer{StatusCode: 300}, 0, Retry},
		{"client error", Answer{StatusCode: 410}, 0, Retry},
		{"server error", Answer{StatusCode: 500}, 0, Retry},
		{"no answer", Answer{Err: errors.New("timeout")}, 0, Retry},
		{"status with an error", Answer{StatusCode: 200, Err: errors.New("cut")}, 0, Retry},
		{"just before the deadline", Answer{StatusCode: 500}, GiveUpAfter - time.Microsecond, Retry},
		{"at the deadline", Answer{StatusCode: 500}, GiveUpAfter, GiveUp},
		{"no answer after the deadline", Answer{Err: errors.New("refused")}, GiveUpAfter + time.Hour, GiveUp},
	}
	for _, tc := range tests {
		if got := Decide(tc.a, created, created.Add(tc.at)); got != tc.want {
			t.Errorf("%s: Decide() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAnswerReason(t *testing.T) {
	if r := (Answer{StatusCode: 503}).Reason(); r != "answered 503" {
		t.Errorf("Reason() = %q", r)
	}
	if r := (Answer{Err: errors.New("refused")}).Reason(); r != "refused" {
		t.Errorf("Reason() = %q", r)
	}
	if r := (Answer{StatusCode: 200}).Reason(); r != "" {
		t.Errorf("Reason() = %q, want empty", r)
	}
}
