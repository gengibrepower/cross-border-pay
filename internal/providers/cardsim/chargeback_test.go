package cardsim_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
)

// receiver keeps every event the simulator posts, verified with testSecret.
type receiver struct {
	mu     sync.Mutex
	events []cardsim.Event
	bad    int
}

func (rc *receiver) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		e, err := cardsim.VerifyEvent([]byte(testSecret), r.Header.Get(cardsim.SignatureHeader), body, t0)
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if err != nil {
			rc.bad++
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rc.events = append(rc.events, e)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (rc *receiver) types() []string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := make([]string, len(rc.events))
	for i := range rc.events {
		out[i] = string(rc.events[i].Type) + ":" + string(rc.events[i].Chargeback.Status)
	}
	return out
}

func newDisputeSim(t *testing.T) (*cardsim.Client, *receiver) {
	t.Helper()
	rc := &receiver{}
	srv := rc.serve(t)
	client, _, _ := newSimWithWebhook(t, 1, srv.URL)
	return client, rc
}

func TestChargebackOfACapture(t *testing.T) {
	c, rc := newDisputeSim(t)
	a := authorize(t, c, "key-1", cardsim.TokenApproved, true)

	cb, err := c.OpenChargeback(t.Context(), a.ID, cardsim.OpenChargeback{Amount: brl(10150), Reason: " fraudulent "})
	if err != nil {
		t.Fatal(err)
	}
	if cb.Status != cardsim.NeedsResponse || cb.AuthorizationID != a.ID || cb.AuthorizationKey != "key-1" ||
		cb.Amount != brl(10150) || cb.Reason != "fraudulent" || cb.ClosedAt != nil {
		t.Fatalf("chargeback = %+v", cb)
	}
	if got, err := c.GetChargeback(t.Context(), cb.ID); err != nil || got != cb {
		t.Errorf("GetChargeback = %+v, %v; want %+v", got, err, cb)
	}
	if _, err := c.OpenChargeback(t.Context(), a.ID, cardsim.OpenChargeback{Amount: brl(1), Reason: "again"}); !errors.Is(err, cardsim.ErrNotDisputable) {
		t.Errorf("chargeback past the capture: error = %v, want ErrNotDisputable", err)
	}

	contested, err := c.Contest(t.Context(), cb.ID, cardsim.Contest{Evidence: "delivered"})
	if err != nil || contested.Status != cardsim.UnderReview {
		t.Fatalf("Contest = %+v, %v", contested, err)
	}
	if again, err := c.Contest(t.Context(), cb.ID, cardsim.Contest{Evidence: "delivered"}); err != nil || again.Status != cardsim.UnderReview {
		t.Errorf("Contest again = %+v, %v; want it as it was", again, err)
	}
	if _, err := c.Accept(t.Context(), cb.ID); !errors.Is(err, cardsim.ErrChargebackClosed) {
		t.Errorf("Accept under review: error = %v, want ErrChargebackClosed", err)
	}
	won, err := c.ResolveChargeback(t.Context(), cb.ID, cardsim.Won)
	if err != nil || won.Status != cardsim.Won || won.ClosedAt == nil {
		t.Fatalf("Resolve(won) = %+v, %v", won, err)
	}
	if again, err := c.ResolveChargeback(t.Context(), cb.ID, cardsim.Won); err != nil || again.Status != cardsim.Won {
		t.Errorf("Resolve(won) again = %+v, %v", again, err)
	}
	if _, err := c.ResolveChargeback(t.Context(), cb.ID, cardsim.Lost); !errors.Is(err, cardsim.ErrChargebackClosed) {
		t.Errorf("Resolve(lost) after won: error = %v, want ErrChargebackClosed", err)
	}
	// One event per fact: opened, and closed once, whatever was asked again.
	if got := rc.types(); len(got) != 2 || got[0] != "chargeback.opened:needs_response" || got[1] != "chargeback.closed:won" || rc.bad != 0 {
		t.Errorf("events = %v (%d bad signatures)", got, rc.bad)
	}
}

// A refunded capture can still be charged back (decision 7), and in parts, up to the
// capture; a chargeback nobody answers is lost, and cannot be won without a contest.
func TestChargebacksIgnoreRefundsAndAddUpToTheCapture(t *testing.T) {
	c, rc := newDisputeSim(t)
	a := authorize(t, c, "key-2", cardsim.TokenApproved, true)
	if rf, err := c.Refund(t.Context(), "refund-1", a.ID, cardsim.CreateRefund{Amount: brl(10150)}); err != nil || rf.Status != cardsim.RefundCompleted {
		t.Fatalf("Refund = %+v, %v", rf, err)
	}
	first, err := c.OpenChargeback(t.Context(), a.ID, cardsim.OpenChargeback{Amount: brl(5075), Reason: "duplicate"})
	if err != nil {
		t.Fatalf("chargeback of a refunded capture: %v", err)
	}
	second, err := c.OpenChargeback(t.Context(), a.ID, cardsim.OpenChargeback{Amount: brl(5075), Reason: "duplicate"})
	if err != nil || second.ID == first.ID {
		t.Fatalf("second half = %+v, %v; want another chargeback", second, err)
	}
	if _, err := c.ResolveChargeback(t.Context(), first.ID, cardsim.Won); !errors.Is(err, cardsim.ErrChargebackClosed) {
		t.Errorf("won without a contest: error = %v, want ErrChargebackClosed", err)
	}
	lost, err := c.ResolveChargeback(t.Context(), first.ID, cardsim.Lost)
	if err != nil || lost.Status != cardsim.Lost {
		t.Errorf("unanswered, lost = %+v, %v", lost, err)
	}
	accepted, err := c.Accept(t.Context(), second.ID)
	if err != nil || accepted.Status != cardsim.Lost {
		t.Fatalf("Accept = %+v, %v", accepted, err)
	}
	if again, err := c.Accept(t.Context(), second.ID); err != nil || again.Status != cardsim.Lost {
		t.Errorf("Accept again = %+v, %v", again, err)
	}
	if got := rc.types(); len(got) != 4 {
		t.Errorf("events = %v, want two opened and two closed", got)
	}
}

func TestChargebackRefusals(t *testing.T) {
	c, _ := newDisputeSim(t)
	held := authorize(t, c, "key-3", cardsim.TokenApproved, false)
	declined := authorize(t, c, "key-4", cardsim.TokenDeclined, true)
	captured := authorize(t, c, "key-5", cardsim.TokenApproved, true)
	for name, tc := range map[string]struct {
		id   string
		req  cardsim.OpenChargeback
		want error
	}{
		"not captured": {held.ID, cardsim.OpenChargeback{Amount: brl(100), Reason: "r"}, cardsim.ErrNotDisputable},
		"declined":     {declined.ID, cardsim.OpenChargeback{Amount: brl(100), Reason: "r"}, cardsim.ErrNotDisputable},
		"unknown":      {"auth_nope", cardsim.OpenChargeback{Amount: brl(100), Reason: "r"}, cardsim.ErrNotFound},
		"no reason":    {captured.ID, cardsim.OpenChargeback{Amount: brl(100), Reason: " "}, nil},
		"zero":         {captured.ID, cardsim.OpenChargeback{Amount: brl(0), Reason: "r"}, nil},
	} {
		_, err := c.OpenChargeback(t.Context(), tc.id, tc.req)
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: error = %v, want %v", name, err, tc.want)
		}
	}
	if _, err := c.Accept(t.Context(), "cbk_nope"); !errors.Is(err, cardsim.ErrChargebackNotFound) {
		t.Errorf("Accept(unknown) error = %v", err)
	}
	cb, err := c.OpenChargeback(t.Context(), captured.ID, cardsim.OpenChargeback{Amount: brl(100), Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Contest(t.Context(), cb.ID, cardsim.Contest{Evidence: ""}); err == nil {
		t.Error("contest without evidence accepted")
	}
	if err := c.InjectFault(t.Context(), cardsim.Fault{Op: cardsim.OpRespond, Kind: cardsim.FailBefore, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Accept(t.Context(), cb.ID); err == nil {
		t.Error("Accept with a fault succeeded")
	}
	if got, err := c.GetChargeback(t.Context(), cb.ID); err != nil || got.Status != cardsim.NeedsResponse {
		t.Errorf("after the failed accept = %+v, %v; want needs_response", got, err)
	}
}

// The same seed opens the same chargebacks: their IDs come from it and the
// authorization's key.
func TestChargebackIDsComeFromTheSeed(t *testing.T) {
	ids := func() []string {
		c, _ := newDisputeSim(t)
		a := authorize(t, c, "key-6", cardsim.TokenApproved, true)
		var out []string
		for range 2 {
			cb, err := c.OpenChargeback(t.Context(), a.ID, cardsim.OpenChargeback{Amount: brl(100), Reason: "r"})
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, cb.ID)
		}
		return out
	}
	first, second := ids(), ids()
	if first[0] != second[0] || first[1] != second[1] || first[0] == first[1] {
		t.Errorf("IDs = %v and %v, want the same two, different from each other", first, second)
	}
}
