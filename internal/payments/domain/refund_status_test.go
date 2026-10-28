package domain_test

import (
	"errors"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

type refundWant struct {
	outcome outcome
	to      domain.RefundStatus
}

// expectedRefund is the refund machine written out again, independently of the
// production table: every (status, event) pair not listed must be ErrInvalidTransition.
var expectedRefund = map[domain.RefundStatus]map[domain.RefundEvent]refundWant{
	domain.RefundCreated: {
		domain.SendRefund: {moves, domain.RefundSent},
	},
	domain.RefundSent: {
		domain.SendRefund:      {duplicate, domain.RefundSent},
		domain.RefundAmbiguous: {moves, domain.RefundUnknown},
		domain.SucceedRefund:   {moves, domain.RefundSucceeded},
		domain.RejectRefund:    {moves, domain.RefundRejected},
	},
	domain.RefundUnknown: {
		domain.RefundAmbiguous: {duplicate, domain.RefundUnknown},
		domain.SucceedRefund:   {moves, domain.RefundSucceeded},
		domain.RejectRefund:    {moves, domain.RefundRejected},
	},
	domain.RefundSucceeded: {
		domain.SucceedRefund: {duplicate, domain.RefundSucceeded},
	},
	domain.RefundRejected: {
		domain.RejectRefund: {duplicate, domain.RefundRejected},
	},
}

func TestTransitionRefundIsExhaustive(t *testing.T) {
	if len(expectedRefund) != len(domain.RefundStatuses()) {
		t.Fatalf("expected covers %d statuses, domain has %d", len(expectedRefund), len(domain.RefundStatuses()))
	}
	for _, from := range domain.RefundStatuses() {
		for _, e := range domain.RefundEvents() {
			w := expectedRefund[from][e]
			to, changed, err := domain.TransitionRefund(from, e)
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
			}
		}
	}
}

// Section 4.3: an ambiguous answer never ends a refund, and nothing goes back to created.
func TestAmbiguousNeverEndsARefund(t *testing.T) {
	for _, from := range domain.RefundStatuses() {
		if to, changed, _ := domain.TransitionRefund(from, domain.RefundAmbiguous); changed &&
			(to == domain.RefundRejected || to == domain.RefundSucceeded) {
			t.Errorf("ambiguous on %s ended the refund as %s", from, to)
		}
		for _, e := range domain.RefundEvents() {
			if to, changed, _ := domain.TransitionRefund(from, e); changed && to == domain.RefundCreated {
				t.Errorf("%s on %s went back to created", e, from)
			}
		}
	}
}

func TestParseRefundStatus(t *testing.T) {
	for _, s := range domain.RefundStatuses() {
		if got, err := domain.ParseRefundStatus(string(s)); err != nil || got != s {
			t.Errorf("ParseRefundStatus(%q) = %q, %v", s, got, err)
		}
	}
	if _, err := domain.ParseRefundStatus("pending"); !errors.Is(err, domain.ErrUnknownRefundStatus) {
		t.Errorf("ParseRefundStatus(pending) error = %v, want %v", err, domain.ErrUnknownRefundStatus)
	}
}
