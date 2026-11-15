package boletosim_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/providers/boletosim"
)

// A boleto paid at 23:30 in São Paulo is that day's, though the date in UTC has turned.
func TestStatementHasTheDaysMoney(t *testing.T) {
	s := start(t, &bank)
	b := s.create(t, "att_1", request(t, 10150, "2026-11-16"))
	s.create(t, "att_2", request(t, 5000, "2026-11-16"))
	s.clock.Advance(14*time.Hour + 30*time.Minute) // Friday 13, 23:30 in São Paulo
	paid, err := s.client.Pay(t.Context(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	rf, err := s.client.Refund(t.Context(), "ref_1", b.ID, boletosim.CreateRefund{Amount: money.New(5075, money.BRL)})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.client.Statement(t.Context(), day(t, "2026-11-13"))
	if err != nil {
		t.Fatal(err)
	}
	want := []boletosim.Movement{
		{Kind: "received", ID: b.ID, IdempotencyKey: "att_1", Amount: paid.Amount, At: paid.PaidAt.UTC()},
		{Kind: "refunded", ID: rf.ID, IdempotencyKey: "ref_1", ParentID: b.ID, Amount: rf.Amount, At: rf.CreatedAt.UTC()},
	}
	if !reflect.DeepEqual(got.Movements, want) {
		t.Errorf("statement = %+v\nwant %+v", got.Movements, want)
	}
	if next, err := s.client.Statement(t.Context(), calendar.On(paid.PaidAt.UTC(), time.UTC)); err != nil || len(next.Movements) != 0 {
		t.Errorf("statement of the UTC date = %+v, %v; want empty", next, err)
	}
}
