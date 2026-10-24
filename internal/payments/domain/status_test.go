package domain_test

import (
	"errors"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

type outcome int

const (
	invalid outcome = iota
	moves
	duplicate
	late
)

type want struct {
	outcome outcome
	to      domain.Status
}

// expected is section 3.4 written out again, independently of the production table:
// every (status, event) pair not listed here must be ErrInvalidTransition.
var expected = map[domain.Status]map[domain.Event]want{
	domain.Pending: {
		domain.Confirm: {moves, domain.Paid},
		domain.Expire:  {moves, domain.Expired},
		domain.Cancel:  {moves, domain.Canceled},
	},
	domain.Paid: {
		domain.Confirm:       {duplicate, domain.Paid},
		domain.RefundPartial: {moves, domain.PartiallyRefunded},
		domain.RefundFull:    {moves, domain.Refunded},
		domain.Chargeback:    {moves, domain.ChargedBack},
	},
	domain.PartiallyRefunded: {
		domain.Confirm:       {duplicate, domain.PartiallyRefunded},
		domain.RefundPartial: {moves, domain.PartiallyRefunded},
		domain.RefundFull:    {moves, domain.Refunded},
		domain.Chargeback:    {moves, domain.ChargedBack},
	},
	domain.Refunded: {
		domain.Confirm:    {duplicate, domain.Refunded},
		domain.RefundFull: {duplicate, domain.Refunded},
		domain.Chargeback: {moves, domain.ChargedBack},
	},
	domain.ChargedBack: {
		domain.Confirm:    {duplicate, domain.ChargedBack},
		domain.Chargeback: {duplicate, domain.ChargedBack},
	},
	domain.Expired: {
		domain.Confirm: {late, domain.Expired},
		domain.Expire:  {duplicate, domain.Expired},
	},
	domain.Canceled: {
		domain.Confirm: {late, domain.Canceled},
		domain.Cancel:  {duplicate, domain.Canceled},
	},
}

// F3 done criterion: every transition outside the table returns ErrInvalidTransition.
func TestTransitionIsExhaustive(t *testing.T) {
	if len(expected) != len(domain.Statuses()) {
		t.Fatalf("expected covers %d statuses, domain has %d", len(expected), len(domain.Statuses()))
	}
	for _, from := range domain.Statuses() {
		for _, e := range domain.Events() {
			w := expected[from][e]
			to, changed, err := domain.Transition(from, e)

			switch w.outcome {
			case moves:
				if err != nil || !changed || to != w.to {
					t.Errorf("%s on %s = (%s, %v, %v), want move to %s", e, from, to, changed, err, w.to)
				}
			case duplicate:
				if err != nil || changed || to != from {
					t.Errorf("%s on %s = (%s, %v, %v), want no-op", e, from, to, changed, err)
				}
			case late:
				if !errors.Is(err, domain.ErrLatePayment) || !errors.Is(err, domain.ErrInvalidTransition) || to != from {
					t.Errorf("%s on %s = (%s, %v, %v), want ErrLatePayment", e, from, to, changed, err)
				}
			case invalid:
				if !errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrLatePayment) || changed || to != from {
					t.Errorf("%s on %s = (%s, %v, %v), want ErrInvalidTransition", e, from, to, changed, err)
				}
			}
		}
	}
}

func TestUnknownStatusAndEventAreInvalid(t *testing.T) {
	if _, _, err := domain.Transition("settled", domain.Confirm); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("unknown status: error = %v, want %v", err, domain.ErrInvalidTransition)
	}
	if _, _, err := domain.Transition(domain.Pending, domain.Event(0)); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("zero event: error = %v, want %v", err, domain.ErrInvalidTransition)
	}
}

func TestTerminalStatusesOnlyAcceptDuplicates(t *testing.T) {
	for _, from := range []domain.Status{domain.ChargedBack, domain.Expired, domain.Canceled} {
		for _, e := range domain.Events() {
			if _, changed, _ := domain.Transition(from, e); changed {
				t.Errorf("%s left terminal status %s", e, from)
			}
		}
	}
}

func TestParseStatus(t *testing.T) {
	for _, s := range domain.Statuses() {
		got, err := domain.ParseStatus(string(s))
		if err != nil || got != s {
			t.Errorf("ParseStatus(%q) = %q, %v", s, got, err)
		}
	}
	if _, err := domain.ParseStatus("PAID"); !errors.Is(err, domain.ErrUnknownStatus) {
		t.Errorf("ParseStatus(PAID) error = %v, want %v", err, domain.ErrUnknownStatus)
	}
}
