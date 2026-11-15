package boleto

import (
	"fmt"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
)

const (
	minFactor = 1000
	cycle     = 9000
)

// cycleStart is a date of factor 1000. The factor counted days from 7 October 1997 and
// reached 9999 on 21 February 2025; Febraban restarted it at 1000 on 22 February 2025,
// 9,000 days after the first 1000, so one count from that day covers both bases.
var cycleStart = mustDate("2000-07-03")

// DueFactor refuses a date before the first 1000: 0000 is a boleto with no due date,
// which decision 53 does not issue.
func DueFactor(due calendar.Date) (int, error) {
	if due.IsZero() || due.Before(cycleStart) {
		return 0, fmt.Errorf("%w: %s", ErrInvalidDue, due)
	}
	return minFactor + due.DaysSince(cycleStart)%cycle, nil
}

func dueNear(factor int, near calendar.Date) calendar.Date {
	offset := factor - minFactor
	cycles := (near.DaysSince(cycleStart) - offset + cycle/2) / cycle
	if cycles < 0 {
		cycles = 0
	}
	return cycleStart.AddDays(offset + cycles*cycle)
}

func mustDate(s string) calendar.Date {
	d, err := calendar.ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}
