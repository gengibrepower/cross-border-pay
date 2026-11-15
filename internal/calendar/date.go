// Package calendar is decision 21: a static table versioned in the code. Municipal
// holidays are out of scope (section 10).
package calendar

import (
	"errors"
	"fmt"
	"time"
)

var ErrInvalidDate = errors.New("calendar: invalid date")

// Date has no time or zone. The zero value is not a valid date.
type Date struct {
	year  int
	month time.Month
	day   int
}

func NewDate(year int, month time.Month, day int) (Date, error) {
	d := dateOf(time.Date(year, month, day, 0, 0, 0, 0, time.UTC))
	if d.year != year || d.month != month || d.day != day {
		return Date{}, fmt.Errorf("%w: %04d-%02d-%02d", ErrInvalidDate, year, month, day)
	}
	return d, nil
}

func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("%w: %q", ErrInvalidDate, s)
	}
	return dateOf(t), nil
}

// On: the same instant is two dates in two zones.
func On(t time.Time, loc *time.Location) Date {
	return dateOf(t.In(loc))
}

func dateOf(t time.Time) Date {
	y, m, d := t.Date()
	return Date{year: y, month: m, day: d}
}

func (d Date) Year() int { return d.year }

func (d Date) Month() time.Month { return d.month }

func (d Date) Day() int { return d.day }

func (d Date) IsZero() bool { return d == Date{} }

func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.year, d.month, d.day) }

func (d Date) Weekday() time.Weekday { return d.midnight(time.UTC).Weekday() }

func (d Date) AddDays(n int) Date {
	return dateOf(time.Date(d.year, d.month, d.day+n, 0, 0, 0, 0, time.UTC))
}

func (d Date) DaysSince(from Date) int {
	return int(d.midnight(time.UTC).Sub(from.midnight(time.UTC)).Hours() / 24)
}

func (d Date) Before(o Date) bool { return d.DaysSince(o) < 0 }

func (d Date) After(o Date) bool { return d.DaysSince(o) > 0 }

// StartIn and EndIn: a boleto due on d can be paid at any instant before d.EndIn(loc).
func (d Date) StartIn(loc *time.Location) time.Time { return d.midnight(loc) }

func (d Date) EndIn(loc *time.Location) time.Time { return d.AddDays(1).midnight(loc) }

func (d Date) midnight(loc *time.Location) time.Time {
	return time.Date(d.year, d.month, d.day, 0, 0, 0, 0, loc)
}

func (d Date) MarshalText() ([]byte, error) {
	if d.IsZero() {
		return nil, fmt.Errorf("%w: zero date", ErrInvalidDate)
	}
	return []byte(d.String()), nil
}

func (d *Date) UnmarshalText(b []byte) error {
	parsed, err := ParseDate(string(b))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}
