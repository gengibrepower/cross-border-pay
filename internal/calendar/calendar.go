package calendar

import (
	"fmt"
	"time"
	_ "time/tzdata" // the zone must load in a container without zoneinfo
)

// Calendar is one country's banking days. A change to a table is a new Version, so a
// date computed before and after can be told apart.
type Calendar struct {
	version  string
	location *time.Location
	fixed    []fixedHoliday
	// movable are days relative to Easter Sunday: -2 is Good Friday.
	movable []int
}

type fixedHoliday struct {
	month time.Month
	day   int
	// since zero means always.
	since int
}

// BrazilVersion names the table Brazil returns. BR-2024 is Lei 14.759/2023, which
// made 20 November a national holiday from 2024 on.
const BrazilVersion = "BR-2024"

// Brazil counts Carnival Monday and Tuesday, when banks do not open, and 31 December,
// when they do not serve the public.
func Brazil() (Calendar, error) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		return Calendar{}, fmt.Errorf("calendar: %w", err)
	}
	return Calendar{
		version:  BrazilVersion,
		location: loc,
		fixed: []fixedHoliday{
			{month: time.January, day: 1},
			{month: time.April, day: 21},
			{month: time.May, day: 1},
			{month: time.September, day: 7},
			{month: time.October, day: 12},
			{month: time.November, day: 2},
			{month: time.November, day: 15},
			{month: time.November, day: 20, since: 2024},
			{month: time.December, day: 25},
			{month: time.December, day: 31},
		},
		// Carnival Monday and Tuesday, Good Friday, Corpus Christi.
		movable: []int{-48, -47, -2, 60},
	}, nil
}

func (c *Calendar) Version() string { return c.version }

func (c *Calendar) Location() *time.Location { return c.location }

func (c *Calendar) Today(t time.Time) Date { return On(t, c.location) }

func (c *Calendar) IsHoliday(d Date) bool {
	for _, h := range c.fixed {
		if h.month == d.month && h.day == d.day && d.year >= h.since {
			return true
		}
	}
	easter := Easter(d.year)
	for _, offset := range c.movable {
		if easter.AddDays(offset) == d {
			return true
		}
	}
	return false
}

func (c *Calendar) IsBusinessDay(d Date) bool {
	if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	return !c.IsHoliday(d)
}

// OnOrAfter is where a due date that falls on a holiday is paid.
func (c *Calendar) OnOrAfter(d Date) Date {
	for !c.IsBusinessDay(d) {
		d = d.AddDays(1)
	}
	return d
}

// AddBusinessDays doesn't count d: D+2 from a Friday is the Tuesday. n must not be
// negative.
func (c *Calendar) AddBusinessDays(d Date, n int) Date {
	for n > 0 {
		d = d.AddDays(1)
		if c.IsBusinessDay(d) {
			n--
		}
	}
	return d
}

// Easter is the anonymous algorithm in Meeus, Astronomical Algorithms, chapter 8.
func Easter(year int) Date {
	a := year % 19
	b, c := year/100, year%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return Date{year: year, month: time.Month(month), day: day}
}
