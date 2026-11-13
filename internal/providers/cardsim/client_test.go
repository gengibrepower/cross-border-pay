package cardsim_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim/server"
)

var t0 = time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)

func brl(n int64) money.Money { return money.New(n, money.BRL) }

func newSim(t *testing.T, seed uint64) (*cardsim.Client, *clock.Fake) {
	t.Helper()
	c := clock.NewFake(t0)
	mux := http.NewServeMux()
	server.New(&server.Config{Seed: seed}, c, slog.New(slog.DiscardHandler)).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := cardsim.NewClient(srv.URL, &http.Client{Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return client, c
}

func authorize(t *testing.T, c *cardsim.Client, key, token string, capture bool) cardsim.Authorization {
	t.Helper()
	a, err := c.Authorize(t.Context(), key, cardsim.Authorize{Amount: brl(10150), Token: token, Capture: capture})
	if err != nil {
		t.Fatalf("Authorize(%s) error = %v", key, err)
	}
	return a
}

// The token decides the issuer's answer, and a decline is an answer, not an error.
func TestTokensDecideTheAnswer(t *testing.T) {
	c, _ := newSim(t, 1)
	for _, tc := range []struct {
		token   string
		capture bool
		status  cardsim.Status
		reason  string
	}{
		{cardsim.TokenApproved, true, cardsim.Captured, ""},
		{cardsim.TokenApproved, false, cardsim.Authorized, ""},
		{cardsim.TokenDeclined, true, cardsim.Declined, "card_declined"},
		{cardsim.TokenInsufficientFunds, true, cardsim.Declined, "insufficient_funds"},
		{"tok_unknown", true, cardsim.Declined, "invalid_token"},
	} {
		a := authorize(t, c, fmt.Sprintf("%s-%v", tc.token, tc.capture), tc.token, tc.capture)
		if a.Status != tc.status || a.DeclineReason != tc.reason {
			t.Errorf("%s capture=%v = %s %q, want %s %q", tc.token, tc.capture, a.Status, a.DeclineReason, tc.status, tc.reason)
		}
		wantCaptured := int64(0)
		if a.Status == cardsim.Captured {
			wantCaptured = 10150
		}
		if a.Captured.Amount() != wantCaptured || (a.CapturedAt != nil) != (wantCaptured > 0) {
			t.Errorf("%s capture=%v captured %s at %v", tc.token, tc.capture, a.Captured, a.CapturedAt)
		}
		if !a.ExpiresAt.Equal(t0.Add(cardsim.AuthorizationValidity)) {
			t.Errorf("expires_at = %v, want 7 days after creation", a.ExpiresAt)
		}
	}
	if err := c.InjectFault(t.Context(), cardsim.Fault{Op: cardsim.OpAuthorize, Kind: cardsim.Reject, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if a := authorize(t, c, "rejected", cardsim.TokenApproved, true); a.Status != cardsim.Declined || a.DeclineReason != "do_not_honor" {
		t.Errorf("rejected by fault = %s %q, want declined do_not_honor", a.Status, a.DeclineReason)
	}
}

// One authorization per key; the same key with another request is refused, and a
// card number never gets in.
func TestAuthorizeIsIdempotentPerKey(t *testing.T) {
	c, _ := newSim(t, 1)
	first := authorize(t, c, "k", cardsim.TokenApproved, true)
	if again := authorize(t, c, "k", cardsim.TokenApproved, true); again.ID != first.ID || again.Status != first.Status || again.Captured != first.Captured {
		t.Errorf("same key again = %+v, want %+v", again, first)
	}
	_, err := c.Authorize(t.Context(), "k", cardsim.Authorize{Amount: brl(1), Token: cardsim.TokenApproved, Capture: true})
	if !errors.Is(err, cardsim.ErrKeyReused) {
		t.Errorf("same key, other amount error = %v, want %v", err, cardsim.ErrKeyReused)
	}
	for name, req := range map[string]cardsim.Authorize{
		"card number": {Amount: brl(100), Token: "4111111111111111"},
		"in dollars":  {Amount: money.New(100, money.USD), Token: cardsim.TokenApproved},
		"zero":        {Amount: brl(0), Token: cardsim.TokenApproved},
	} {
		if _, err := c.Authorize(t.Context(), "bad-"+name, req); !errors.Is(err, cardsim.ErrBadResponse) {
			t.Errorf("%s: error = %v, want a 400", name, err)
		}
	}
}

// The same seed and keys give the same authorizations whatever order they arrive in.
func TestSameSeedSameAuthorizations(t *testing.T) {
	const n = 50
	run := func(concurrent bool) map[string]string {
		c, _ := newSim(t, 7)
		ids := make(map[string]string, n)
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := range n {
			key := fmt.Sprintf("key-%d", i)
			if !concurrent {
				ids[key] = authorize(t, c, key, cardsim.TokenApproved, true).ID
				continue
			}
			key = fmt.Sprintf("key-%d", n-1-i)
			wg.Go(func() {
				a, err := c.Authorize(context.WithoutCancel(t.Context()), key, cardsim.Authorize{Amount: brl(10150), Token: cardsim.TokenApproved, Capture: true})
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				ids[key] = a.ID
				mu.Unlock()
			})
		}
		wg.Wait()
		return ids
	}
	inOrder, reversed := run(false), run(true)
	for key, id := range inOrder {
		if reversed[key] != id {
			t.Errorf("%s: %s in order, %s concurrent in reverse", key, id, reversed[key])
		}
	}
}

// The authorization exists after a timeout_after, and the lookup by key finds it.
func TestTimeoutAfterAuthorizesAndGetFindsIt(t *testing.T) {
	c, _ := newSim(t, 1)
	if err := c.InjectFault(t.Context(), cardsim.Fault{Op: cardsim.OpAuthorize, Kind: cardsim.TimeoutAfter, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Authorize(t.Context(), "k", cardsim.Authorize{Amount: brl(10150), Token: cardsim.TokenApproved, Capture: true}); err == nil {
		t.Fatal("Authorize() answered through a timeout_after")
	}
	a, err := c.Get(t.Context(), "k")
	if err != nil || a.Status != cardsim.Captured {
		t.Errorf("Get() = %+v, %v; want captured", a, err)
	}
	if _, err := c.Get(t.Context(), "never"); !errors.Is(err, cardsim.ErrNotFound) {
		t.Errorf("Get(never) error = %v, want %v", err, cardsim.ErrNotFound)
	}
}

// A capture charges up to what was authorized, once; a retry of the same capture
// gets the same answer, another amount is refused, and seven days on nothing is left
// to capture.
func TestCaptureOnceWithinTheValidity(t *testing.T) {
	c, clk := newSim(t, 1)
	a := authorize(t, c, "k", cardsim.TokenApproved, false)
	if _, err := c.Capture(t.Context(), a.ID, cardsim.Capture{Amount: brl(10151)}); !errors.Is(err, cardsim.ErrNotCapturable) {
		t.Errorf("capture above the authorization error = %v, want %v", err, cardsim.ErrNotCapturable)
	}
	clk.Advance(time.Hour)
	got, err := c.Capture(t.Context(), a.ID, cardsim.Capture{Amount: brl(5000)})
	if err != nil || got.Status != cardsim.Captured || got.Captured != brl(5000) || !got.CapturedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("partial capture = %+v, %v", got, err)
	}
	if again, err := c.Capture(t.Context(), a.ID, cardsim.Capture{Amount: brl(5000)}); err != nil || again.Captured != brl(5000) {
		t.Errorf("same capture again = %+v, %v", again, err)
	}
	if _, err := c.Capture(t.Context(), a.ID, cardsim.Capture{Amount: brl(6000)}); !errors.Is(err, cardsim.ErrNotCapturable) {
		t.Errorf("second capture of another amount error = %v, want %v", err, cardsim.ErrNotCapturable)
	}

	late := authorize(t, c, "late", cardsim.TokenApproved, false)
	clk.Advance(cardsim.AuthorizationValidity)
	if _, err := c.Capture(t.Context(), late.ID, cardsim.Capture{Amount: brl(10150)}); !errors.Is(err, cardsim.ErrNotCapturable) {
		t.Errorf("capture at the deadline error = %v, want %v", err, cardsim.ErrNotCapturable)
	}
	if got, err := c.Get(t.Context(), "late"); err != nil || got.Status != cardsim.Expired {
		t.Errorf("Get(late) = %s, %v; want expired", got.Status, err)
	}
	declined := authorize(t, c, "declined", cardsim.TokenDeclined, false)
	if _, err := c.Capture(t.Context(), declined.ID, cardsim.Capture{Amount: brl(1)}); !errors.Is(err, cardsim.ErrNotCapturable) {
		t.Errorf("capture of a decline error = %v, want %v", err, cardsim.ErrNotCapturable)
	}
	if _, err := c.Capture(t.Context(), "auth_none", cardsim.Capture{Amount: brl(1)}); !errors.Is(err, cardsim.ErrNotFound) {
		t.Errorf("capture of an unknown authorization error = %v, want %v", err, cardsim.ErrNotFound)
	}
}

// A void releases a live authorization once; nothing captured or closed is voided,
// and a voided authorization cannot be captured.
func TestVoidReleasesOnlyALiveAuthorization(t *testing.T) {
	c, clk := newSim(t, 1)
	a := authorize(t, c, "k", cardsim.TokenApproved, false)
	got, err := c.Void(t.Context(), a.ID)
	if err != nil || got.Status != cardsim.Voided {
		t.Fatalf("Void() = %+v, %v; want voided", got, err)
	}
	if again, err := c.Void(t.Context(), a.ID); err != nil || again.Status != cardsim.Voided {
		t.Errorf("Void() again = %s, %v; want the same voided authorization", again.Status, err)
	}
	if _, err := c.Capture(t.Context(), a.ID, cardsim.Capture{Amount: brl(10150)}); !errors.Is(err, cardsim.ErrNotCapturable) {
		t.Errorf("capture of a voided authorization error = %v, want %v", err, cardsim.ErrNotCapturable)
	}
	if got, err := c.Get(t.Context(), "k"); err != nil || got.Status != cardsim.Voided {
		t.Errorf("Get() = %s, %v; want voided", got.Status, err)
	}

	captured := authorize(t, c, "captured", cardsim.TokenApproved, true)
	declined := authorize(t, c, "declined", cardsim.TokenDeclined, false)
	expired := authorize(t, c, "expired", cardsim.TokenApproved, false)
	clk.Advance(cardsim.AuthorizationValidity)
	for _, id := range []string{captured.ID, declined.ID, expired.ID} {
		if _, err := c.Void(t.Context(), id); !errors.Is(err, cardsim.ErrNotVoidable) {
			t.Errorf("Void(%s) error = %v, want %v", id, err, cardsim.ErrNotVoidable)
		}
	}
	if _, err := c.Void(t.Context(), "auth_none"); !errors.Is(err, cardsim.ErrNotFound) {
		t.Errorf("Void(unknown) error = %v, want %v", err, cardsim.ErrNotFound)
	}
}

// Invariant 8 on the simulator's side: concurrent refunds never return more than was
// captured, and a refusal is stored for the lookup.
func TestConcurrentRefundsNeverPassTheCapture(t *testing.T) {
	c, _ := newSim(t, 1)
	a := authorize(t, c, "k", cardsim.TokenApproved, true)
	var mu sync.Mutex
	completed := 0
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			rf, err := c.Refund(t.Context(), fmt.Sprintf("r%d", i), a.ID, cardsim.CreateRefund{Amount: brl(1000)})
			switch {
			case err == nil && rf.Status == cardsim.RefundCompleted:
				mu.Lock()
				completed++
				mu.Unlock()
			case errors.Is(err, cardsim.ErrRefundRejected):
			default:
				t.Errorf("refund r%d = %+v, %v", i, rf, err)
			}
		})
	}
	wg.Wait()
	if completed != 10 {
		t.Errorf("%d refunds of R$ 10.00 completed on R$ 101.50, want 10", completed)
	}
	rf, err := c.GetRefund(t.Context(), "r0")
	if err != nil || rf.AuthorizationID != a.ID {
		t.Errorf("GetRefund(r0) = %+v, %v", rf, err)
	}

	open := authorize(t, c, "open", cardsim.TokenApproved, false)
	if _, err := c.Refund(t.Context(), "uncaptured", open.ID, cardsim.CreateRefund{Amount: brl(1)}); !errors.Is(err, cardsim.ErrRefundRejected) {
		t.Errorf("refund of an uncaptured authorization error = %v, want %v", err, cardsim.ErrRefundRejected)
	}
	if got, err := c.GetRefund(t.Context(), "uncaptured"); err != nil || got.Status != cardsim.RefundRejected {
		t.Errorf("stored refusal = %+v, %v", got, err)
	}
	if _, err := c.GetRefund(t.Context(), "never"); !errors.Is(err, cardsim.ErrRefundNotFound) {
		t.Errorf("GetRefund(never) error = %v, want %v", err, cardsim.ErrRefundNotFound)
	}
}

// An injected 500 is ambiguous for the client, never a decline or a refusal.
func TestFailBeforeIsNeitherDeclineNorRefusal(t *testing.T) {
	c, _ := newSim(t, 1)
	for _, op := range []cardsim.Op{cardsim.OpAuthorize, cardsim.OpRefund} {
		if err := c.InjectFault(t.Context(), cardsim.Fault{Op: op, Kind: cardsim.FailBefore, Count: 1}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.Authorize(t.Context(), "k", cardsim.Authorize{Amount: brl(10150), Token: cardsim.TokenApproved, Capture: true})
	if !errors.Is(err, cardsim.ErrBadResponse) {
		t.Errorf("Authorize() error = %v, want %v", err, cardsim.ErrBadResponse)
	}
	if _, err := c.Get(t.Context(), "k"); !errors.Is(err, cardsim.ErrNotFound) {
		t.Errorf("a fail_before stored the authorization: %v", err)
	}
	a := authorize(t, c, "k2", cardsim.TokenApproved, true)
	if _, err := c.Refund(t.Context(), "r", a.ID, cardsim.CreateRefund{Amount: brl(1)}); !errors.Is(err, cardsim.ErrBadResponse) {
		t.Errorf("Refund() error = %v, want %v", err, cardsim.ErrBadResponse)
	}
	if err := c.InjectFault(t.Context(), cardsim.Fault{Op: cardsim.OpCapture, Kind: cardsim.Reject, Count: 1}); err == nil {
		t.Error("a capture fault of kind reject was accepted")
	}
}
