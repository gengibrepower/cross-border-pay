package pixsim_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

// history pays one charge, refunds part of it, has one refund refused and leaves
// another charge unpaid: only the payment and the completed refund are money.
func (s *sim) history(t *testing.T) (paid pixsim.Charge, refund pixsim.Refund) {
	t.Helper()
	paid = s.create(t, "att_1", charge(10150))
	s.create(t, "att_2", charge(5000))
	s.clock.Advance(time.Minute)
	paid, err := s.client.Pay(t.Context(), paid.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.clock.Advance(time.Minute)
	refund, err = s.client.Refund(t.Context(), "ref_1", paid.ID, pixsim.CreateRefund{Amount: money.New(5075, money.BRL)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.client.Refund(t.Context(), "ref_2", paid.ID, pixsim.CreateRefund{Amount: money.New(9999, money.BRL)}); !errors.Is(err, pixsim.ErrRefundRejected) {
		t.Fatalf("refund past what is left: error = %v", err)
	}
	return paid, refund
}

func TestStatementHasTheDaysMoney(t *testing.T) {
	s := start(t, &receiver, time.Second)
	paid, refund := s.history(t)
	day := calendar.On(t0, time.UTC)

	got, err := s.client.Statement(t.Context(), day)
	if err != nil {
		t.Fatal(err)
	}
	want := []pixsim.Movement{
		{Kind: "received", ID: paid.ID, IdempotencyKey: "att_1", Amount: paid.Amount, At: paid.PaidAt.UTC()},
		{Kind: "refunded", ID: refund.ID, IdempotencyKey: "ref_1", ParentID: paid.ID, Amount: refund.Amount, At: refund.CreatedAt.UTC()},
	}
	if got.Date != day || !reflect.DeepEqual(got.Movements, want) {
		t.Errorf("Statement(%s) = %+v\nwant %+v", day, got, want)
	}
	if next, err := s.client.Statement(t.Context(), day.AddDays(1)); err != nil || len(next.Movements) != 0 {
		t.Errorf("next day's statement = %+v, %v; want empty", next, err)
	}
}

// rawStatement is the statement's bytes, as the gateway receives them.
func (s *sim) rawStatement(t *testing.T, date string) (status int, body []byte) {
	t.Helper()
	resp, err := http.Get(s.url + "/statements/" + date) //nolint:noctx // a test against a local server
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	if err := errors.Join(err, resp.Body.Close()); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func TestSameSeedSameStatement(t *testing.T) {
	a, b := start(t, &receiver, time.Second), start(t, &receiver, time.Second)
	a.history(t)
	b.history(t)
	day := calendar.On(t0, time.UTC).String()
	_, x := a.rawStatement(t, day)
	_, y := b.rawStatement(t, day)
	if !json.Valid(x) || !bytes.Equal(x, y) {
		t.Errorf("same seed, different statements:\n%s\n%s", x, y)
	}
}

func TestStatementFaultsAndBadDate(t *testing.T) {
	s := start(t, &receiver, time.Second)
	s.fault(t, pixsim.Fault{Op: pixsim.OpStatement, Kind: pixsim.FailBefore, Count: 1})
	day := calendar.On(t0, time.UTC)
	if _, err := s.client.Statement(t.Context(), day); !errors.Is(err, pixsim.ErrBadResponse) {
		t.Errorf("Statement() under fail_before: error = %v, want %v", err, pixsim.ErrBadResponse)
	}
	if _, err := s.client.Statement(t.Context(), day); err != nil {
		t.Errorf("Statement() after the fault was used: %v", err)
	}
	if err := s.client.InjectFault(t.Context(), pixsim.Fault{Op: pixsim.OpStatement, Kind: pixsim.Reject, Count: 1}); err == nil {
		t.Error("a statement accepted a reject fault")
	}
	if status, body := s.rawStatement(t, "2026-13-01"); status != http.StatusBadRequest {
		t.Errorf("bad date: %d %s, want 400", status, body)
	}
}
