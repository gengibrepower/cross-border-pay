package cardsim_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
)

// The capture is the money in, not the authorization; a won chargeback comes back, a
// lost one does not, and an authorization never captured is not money.
func TestStatementHasTheDaysMoney(t *testing.T) {
	c, _ := newDisputeSim(t)
	a := authorize(t, c, "att_1", cardsim.TokenApproved, false)
	authorize(t, c, "att_2", cardsim.TokenApproved, false)
	captured, err := c.Capture(t.Context(), a.ID, cardsim.Capture{Amount: brl(8000)})
	if err != nil {
		t.Fatal(err)
	}
	rf, err := c.Refund(t.Context(), "ref_1", a.ID, cardsim.CreateRefund{Amount: brl(1000)})
	if err != nil {
		t.Fatal(err)
	}
	won, err := c.OpenChargeback(t.Context(), a.ID, cardsim.OpenChargeback{Amount: brl(3000), Reason: "fraudulent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Contest(t.Context(), won.ID, cardsim.Contest{Evidence: "delivered"}); err != nil {
		t.Fatal(err)
	}
	if won, err = c.ResolveChargeback(t.Context(), won.ID, cardsim.Won); err != nil {
		t.Fatal(err)
	}
	lost, err := c.OpenChargeback(t.Context(), a.ID, cardsim.OpenChargeback{Amount: brl(2000), Reason: "not received"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Accept(t.Context(), lost.ID); err != nil {
		t.Fatal(err)
	}

	got, err := c.Statement(t.Context(), calendar.On(t0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := []cardsim.Movement{
		{Kind: "received", ID: a.ID, IdempotencyKey: "att_1", Amount: brl(8000), At: captured.CapturedAt.UTC()},
		{Kind: "charged_back", ID: won.ID, ParentID: a.ID, Amount: brl(3000), At: won.CreatedAt.UTC()},
		{Kind: "reversed", ID: won.ID, ParentID: a.ID, Amount: brl(3000), At: won.ClosedAt.UTC()},
		{Kind: "charged_back", ID: lost.ID, ParentID: a.ID, Amount: brl(2000), At: lost.CreatedAt.UTC()},
		{Kind: "refunded", ID: rf.ID, IdempotencyKey: "ref_1", ParentID: a.ID, Amount: brl(1000), At: rf.CreatedAt.UTC()},
	}
	byID := func(ms []cardsim.Movement) map[string]cardsim.Movement {
		out := map[string]cardsim.Movement{}
		for _, m := range ms {
			out[m.ID+string(m.Kind)] = m
		}
		return out
	}
	if !reflect.DeepEqual(byID(got.Movements), byID(want)) || len(got.Movements) != len(want) {
		t.Errorf("statement = %+v\nwant %+v", got.Movements, want)
	}
}
