package calendar_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
)

func date(t *testing.T, s string) calendar.Date {
	t.Helper()
	d, err := calendar.ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func brazil(t testing.TB) calendar.Calendar {
	t.Helper()
	c, err := calendar.Brazil()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEaster(t *testing.T) {
	// Published dates of Easter Sunday, including both ends of its range (22 March in
	// 2285, 25 April in 2038) and years that stress the century terms.
	for _, want := range []string{
		"1961-04-02", "2000-04-23", "2008-03-23", "2019-04-21", "2024-03-31",
		"2025-04-20", "2026-04-05", "2027-03-28", "2038-04-25", "2100-03-28", "2285-03-22",
	} {
		d := date(t, want)
		if got := calendar.Easter(d.Year()); got != d {
			t.Errorf("Easter(%d) = %s, want %s", d.Year(), got, want)
		}
	}
}

func TestBrazilHolidays(t *testing.T) {
	c := brazil(t)
	holidays := []string{
		"2026-01-01",
		"2026-02-16", "2026-02-17", // Carnival
		"2026-04-03", // Good Friday
		"2026-04-21", "2026-05-01",
		"2026-06-04", // Corpus Christi
		"2026-09-07", "2026-10-12", "2026-11-02", "2026-11-15", "2026-11-20", "2026-12-25", "2026-12-31",
		"2025-03-03", "2025-03-04", "2025-04-18", "2025-06-19",
		"2024-11-20",
	}
	for _, s := range holidays {
		if !c.IsHoliday(date(t, s)) {
			t.Errorf("%s is not a holiday", s)
		}
	}
	notHolidays := []string{
		"2026-02-18", // Ash Wednesday: banks open at noon
		"2026-04-02", "2026-04-05", "2026-11-19",
		"2023-11-20", // before Lei 14.759/2023
	}
	for _, s := range notHolidays {
		if c.IsHoliday(date(t, s)) {
			t.Errorf("%s is a holiday", s)
		}
	}
}

func TestBusinessDays(t *testing.T) {
	c := brazil(t)
	tests := []struct {
		name      string
		from      string
		onOrAfter string
		plus      int
		want      string
	}{
		{name: "a weekday is itself", from: "2026-11-18", onOrAfter: "2026-11-18", plus: 1, want: "2026-11-19"},
		{name: "Saturday moves to Monday", from: "2026-11-14", onOrAfter: "2026-11-16", plus: 2, want: "2026-11-17"},
		{name: "holiday Friday moves past the weekend", from: "2026-11-20", onOrAfter: "2026-11-23", plus: 1, want: "2026-11-23"},
		{name: "D+2 from a Friday is the Tuesday", from: "2026-11-13", onOrAfter: "2026-11-13", plus: 2, want: "2026-11-17"},
		{name: "D+1 over a holiday", from: "2026-11-19", onOrAfter: "2026-11-19", plus: 1, want: "2026-11-23"},
		{name: "year end", from: "2026-12-31", onOrAfter: "2027-01-04", plus: 2, want: "2027-01-05"},
		{name: "Carnival", from: "2026-02-13", onOrAfter: "2026-02-13", plus: 1, want: "2026-02-18"},
		{name: "zero days", from: "2026-11-14", onOrAfter: "2026-11-16", plus: 0, want: "2026-11-14"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from := date(t, tt.from)
			if got := c.OnOrAfter(from); got != date(t, tt.onOrAfter) {
				t.Errorf("OnOrAfter(%s) = %s, want %s", tt.from, got, tt.onOrAfter)
			}
			if got := c.AddBusinessDays(from, tt.plus); got != date(t, tt.want) {
				t.Errorf("AddBusinessDays(%s, %d) = %s, want %s", tt.from, tt.plus, got, tt.want)
			}
		})
	}
}

func TestOnOrAfterProperty(t *testing.T) {
	c := brazil(t)
	start := date(t, "2000-01-01")
	rapid.Check(t, func(t *rapid.T) {
		d := start.AddDays(rapid.IntRange(0, 365*100).Draw(t, "offset"))
		got := c.OnOrAfter(d)
		if !c.IsBusinessDay(got) {
			t.Fatalf("OnOrAfter(%s) = %s, not a business day", d, got)
		}
		if got.DaysSince(d) > 5 {
			t.Fatalf("OnOrAfter(%s) = %s, more than five days later", d, got)
		}
		for x := d; x.Before(got); x = x.AddDays(1) {
			if c.IsBusinessDay(x) {
				t.Fatalf("OnOrAfter(%s) = %s skips business day %s", d, got, x)
			}
		}
		n := rapid.IntRange(0, 30).Draw(t, "n")
		after := c.AddBusinessDays(d, n)
		count := 0
		for x := d.AddDays(1); !x.After(after); x = x.AddDays(1) {
			if c.IsBusinessDay(x) {
				count++
			}
		}
		if count != n || n > 0 && !c.IsBusinessDay(after) {
			t.Fatalf("AddBusinessDays(%s, %d) = %s, which is %d business days later", d, n, after, count)
		}
	})
}

func TestDate(t *testing.T) {
	if _, err := calendar.NewDate(2026, time.February, 29); !errors.Is(err, calendar.ErrInvalidDate) {
		t.Errorf("NewDate(2026-02-29) error = %v, want ErrInvalidDate", err)
	}
	if _, err := calendar.NewDate(2028, time.February, 29); err != nil {
		t.Errorf("NewDate(2028-02-29) error = %v", err)
	}
	if _, err := calendar.ParseDate("14/11/2026"); !errors.Is(err, calendar.ErrInvalidDate) {
		t.Errorf("ParseDate(14/11/2026) error = %v, want ErrInvalidDate", err)
	}

	c := brazil(t)
	late := time.Date(2026, time.November, 15, 2, 0, 0, 0, time.UTC)
	if got := c.Today(late); got != date(t, "2026-11-14") {
		t.Errorf("Today(%s) = %s, want 2026-11-14 in São Paulo", late, got)
	}
	if got := calendar.On(late, time.UTC); got != date(t, "2026-11-15") {
		t.Errorf("On(%s, UTC) = %s, want 2026-11-15", late, got)
	}
	d := date(t, "2026-11-14")
	if got, want := d.EndIn(c.Location()), time.Date(2026, time.November, 15, 3, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("EndIn = %s, want %s", got, want)
	}
	if got, want := d.StartIn(c.Location()), time.Date(2026, time.November, 14, 3, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("StartIn = %s, want %s", got, want)
	}
	if got := date(t, "2025-02-22").DaysSince(date(t, "2000-07-03")); got != 9000 {
		t.Errorf("DaysSince = %d, want 9000", got)
	}
	if got := d.AddDays(-14); got != date(t, "2026-10-31") {
		t.Errorf("AddDays(-14) = %s, want 2026-10-31", got)
	}
}

func TestDateJSON(t *testing.T) {
	in := struct {
		Due calendar.Date `json:"due"`
	}{Due: date(t, "2026-11-17")}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"due":"2026-11-17"}` {
		t.Errorf("Marshal = %s", b)
	}
	var out struct {
		Due calendar.Date `json:"due"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Due != in.Due {
		t.Errorf("Unmarshal = %v, %v", out.Due, err)
	}
	if _, err := json.Marshal(struct{ D calendar.Date }{}); err == nil {
		t.Error("Marshal of the zero date succeeded")
	}
}
