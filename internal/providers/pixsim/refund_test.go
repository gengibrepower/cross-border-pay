package pixsim_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

func refund(amount int64) pixsim.CreateRefund {
	return pixsim.CreateRefund{Amount: money.New(amount, money.BRL)}
}

func (s *sim) paid(t *testing.T, key string, amount int64) pixsim.Charge {
	t.Helper()
	ch := s.create(t, key, charge(amount))
	paid, err := s.client.Pay(t.Context(), ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	return paid
}

func TestRefundIsIdempotentPerKey(t *testing.T) {
	s := start(t, &receiver, time.Second)
	ch := s.paid(t, "att_1", 10150)

	first, err := s.client.Refund(t.Context(), "ref_1", ch.ID, refund(5075))
	if err != nil || first.Status != pixsim.RefundCompleted || !strings.HasPrefix(first.ID, "rfd_") ||
		first.ChargeID != ch.ID || first.Amount != money.New(5075, money.BRL) || !first.CreatedAt.Equal(t0) {
		t.Fatalf("Refund() = %+v, %v; want a completed refund of 50.75", first, err)
	}
	again, err := s.client.Refund(t.Context(), "ref_1", ch.ID, refund(5075))
	if err != nil || again != first {
		t.Errorf("repeated Refund() = %+v, %v; want %+v", again, err, first)
	}
	if _, err := s.client.Refund(t.Context(), "ref_1", ch.ID, refund(100)); !errors.Is(err, pixsim.ErrKeyReused) {
		t.Errorf("Refund() with another amount error = %v, want %v", err, pixsim.ErrKeyReused)
	}
	if got, err := s.client.GetRefund(t.Context(), "ref_1"); err != nil || got != first {
		t.Errorf("GetRefund() = %+v, %v; want %+v", got, err, first)
	}
	if _, err := s.client.GetRefund(t.Context(), "ref_unknown"); !errors.Is(err, pixsim.ErrRefundNotFound) {
		t.Errorf("GetRefund(unknown) error = %v, want %v", err, pixsim.ErrRefundNotFound)
	}
	if other, err := s.client.Refund(t.Context(), "ref_2", ch.ID, refund(5075)); err != nil || other.ID == first.ID {
		t.Errorf("second Refund() = %+v, %v; want its own ID", other, err)
	}
	if _, err := s.client.Refund(t.Context(), "ref_3", "chg_unknown", refund(1)); !errors.Is(err, pixsim.ErrNotFound) {
		t.Errorf("Refund(unknown charge) error = %v, want %v", err, pixsim.ErrNotFound)
	}
}

// The simulator keeps invariant 8 on its side too, and a refused refund is stored, so
// a caller that lost the answer finds the refusal.
func TestRefundRefusals(t *testing.T) {
	s := start(t, &receiver, time.Second)
	paid := s.paid(t, "att_1", 10150)
	unpaid := s.create(t, "att_2", charge(10150))
	if _, err := s.client.Refund(t.Context(), "ref_part", paid.ID, refund(10000)); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, key, charge string
		req               pixsim.CreateRefund
	}{
		{"more than is left", "ref_over", paid.ID, refund(151)},
		{"unpaid charge", "ref_unpaid", unpaid.ID, refund(1)},
		{"another currency", "ref_usd", paid.ID, pixsim.CreateRefund{Amount: money.New(1, money.USD)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.client.Refund(t.Context(), tc.key, tc.charge, tc.req); !errors.Is(err, pixsim.ErrRefundRejected) {
				t.Fatalf("Refund() error = %v, want %v", err, pixsim.ErrRefundRejected)
			}
			if got, err := s.client.GetRefund(t.Context(), tc.key); err != nil || got.Status != pixsim.RefundRejected || got.Reason == "" {
				t.Errorf("GetRefund() = %+v, %v; want rejected with a reason", got, err)
			}
		})
	}
	if rf, err := s.client.Refund(t.Context(), "ref_rest", paid.ID, refund(150)); err != nil || rf.Status != pixsim.RefundCompleted {
		t.Errorf("Refund() of what is left = %+v, %v; want completed", rf, err)
	}
}

func TestConcurrentRefundsNeverExceedTheCharge(t *testing.T) {
	s := start(t, &receiver, 5*time.Second)
	ch := s.paid(t, "att_1", 10150)
	const n = 50
	var mu sync.Mutex
	completed := 0
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			rf, err := s.client.Refund(t.Context(), fmt.Sprintf("ref_%d", i), ch.ID, refund(1000))
			if err == nil && rf.Status == pixsim.RefundCompleted {
				mu.Lock()
				completed++
				mu.Unlock()
			} else if !errors.Is(err, pixsim.ErrRefundRejected) {
				t.Errorf("Refund() error = %v", err)
			}
		})
	}
	wg.Wait()
	if completed != 10 {
		t.Errorf("%d refunds of 10.00 completed on a charge of 101.50, want 10", completed)
	}
}

func TestRefundFaults(t *testing.T) {
	s := start(t, &receiver, 200*time.Millisecond)
	ch := s.paid(t, "att_1", 10150)

	s.fault(t, pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.Reject, Count: 1})
	if _, err := s.client.Refund(t.Context(), "ref_rejected", ch.ID, refund(100)); !errors.Is(err, pixsim.ErrRefundRejected) {
		t.Errorf("Refund() with reject error = %v, want %v", err, pixsim.ErrRefundRejected)
	}

	s.fault(t, pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.FailBefore, Count: 1})
	if _, err := s.client.Refund(t.Context(), "ref_failed", ch.ID, refund(100)); err == nil || errors.Is(err, pixsim.ErrRefundRejected) {
		t.Errorf("Refund() with fail_before error = %v, want an ambiguous error", err)
	}
	if _, err := s.client.GetRefund(t.Context(), "ref_failed"); !errors.Is(err, pixsim.ErrRefundNotFound) {
		t.Errorf("GetRefund() after fail_before error = %v, want %v: nothing was done", err, pixsim.ErrRefundNotFound)
	}

	// The ambiguous case of section 4.3: the money went back and the answer was lost.
	s.fault(t, pixsim.Fault{Op: pixsim.OpRefund, Kind: pixsim.TimeoutAfter, Count: 1})
	if _, err := s.client.Refund(t.Context(), "ref_timeout", ch.ID, refund(100)); err == nil {
		t.Error("Refund() with timeout_after answered")
	}
	if got, err := s.client.GetRefund(t.Context(), "ref_timeout"); err != nil || got.Status != pixsim.RefundCompleted {
		t.Errorf("GetRefund() after timeout_after = %+v, %v; want completed", got, err)
	}

	s.fault(t, pixsim.Fault{Op: pixsim.OpGetRefund, Kind: pixsim.FailBefore, Count: 1})
	if _, err := s.client.GetRefund(t.Context(), "ref_timeout"); err == nil || errors.Is(err, pixsim.ErrRefundNotFound) {
		t.Errorf("GetRefund() with fail_before error = %v, want an ambiguous error", err)
	}
	if err := s.client.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpGetRefund, Kind: pixsim.Reject, Count: 1}); err == nil {
		t.Error("a get_refund reject fault was accepted")
	}
}

func TestSameSeedSameRefunds(t *testing.T) {
	ids := func() string {
		s := start(t, &receiver, time.Second)
		ch := s.paid(t, "att_1", 10150)
		rf, err := s.client.Refund(t.Context(), "ref_1", ch.ID, refund(100))
		if err != nil {
			t.Fatal(err)
		}
		return rf.ID
	}
	if a, b := ids(), ids(); a != b {
		t.Errorf("same seed gave refunds %s and %s", a, b)
	}
}
