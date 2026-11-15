package simkit_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

// The day is São Paulo's (UTC-3): 02:59 UTC is still the day before.
func TestStatementOfKeepsTheDayInOrder(t *testing.T) {
	cal, err := calendar.Brazil()
	if err != nil {
		t.Fatal(err)
	}
	day, err := calendar.NewDate(2026, 11, 3)
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 11, 3, h, m, 0, 0, time.UTC) }
	mv := func(kind simkit.MovementKind, id string, when time.Time) simkit.Movement {
		return simkit.Movement{Kind: kind, ID: id, Amount: money.New(100, money.BRL), At: when.In(cal.Location())}
	}
	all := []simkit.Movement{
		mv(simkit.Received, "b", at(15, 0)),
		mv(simkit.Received, "late", time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)),
		mv(simkit.Reversed, "a", at(15, 0)),
		mv(simkit.Received, "early", at(2, 59)),
		mv(simkit.ChargedBack, "a", at(15, 0)),
		mv(simkit.Received, "first", at(3, 0)),
		mv(simkit.Received, "last", time.Date(2026, 11, 4, 2, 59, 0, 0, time.UTC)),
	}
	got := simkit.StatementOf(day, cal.Location(), all)
	var ids []string
	for _, m := range got.Movements {
		ids = append(ids, m.ID+":"+string(m.Kind))
		if m.At.Location() != time.UTC {
			t.Errorf("%s at %v, want UTC", m.ID, m.At)
		}
	}
	want := []string{"first:received", "a:charged_back", "a:reversed", "b:received", "last:received"}
	if !reflect.DeepEqual(ids, want) || got.Date != day {
		t.Errorf("statement of %s = %v, want %v", got.Date, ids, want)
	}
	reversed := make([]simkit.Movement, len(all))
	for i := range all {
		reversed[len(all)-1-i] = all[i]
	}
	if again := simkit.StatementOf(day, cal.Location(), reversed); !reflect.DeepEqual(again, got) {
		t.Errorf("the input order changed the statement:\n%v\n%v", again, got)
	}
	if empty := simkit.StatementOf(day.AddDays(7), cal.Location(), all); empty.Movements == nil || len(empty.Movements) != 0 {
		t.Errorf("a day without movements = %#v, want an empty list", empty.Movements)
	}
}
