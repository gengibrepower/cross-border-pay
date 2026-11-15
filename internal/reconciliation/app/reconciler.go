// Package app runs the daily reconciliation of section 4.8.
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/reconciliation/domain"
)

// Window is how many days back the other side of a movement is looked for: the gateway
// can learn of a payment days after the provider took it, from the resolver.
// shortcut: a movement learned more than Window days after the provider made it shows
// as missing on both sides; widen it if a provider lags longer.
const Window = 7

type Statements interface {
	Statement(ctx context.Context, provider string, day calendar.Date) ([]domain.Line, error)
}

// Books is the gateway's side, dated by when the gateway learned each movement.
type Books interface {
	Movements(ctx context.Context, provider string, since, until time.Time) ([]domain.Line, error)
	// Settle records a movement only the provider has, the way the provider's own answer
	// would; false is one only a person can place.
	Settle(ctx context.Context, provider string, l *domain.Line, now time.Time) (bool, error)
}

type Cases interface {
	// Held is the money cases hold, which the books leave out.
	Held(ctx context.Context, provider string, since, until time.Time) ([]domain.Line, error)
	Open(ctx context.Context, c *domain.Case) (bool, error)
}

type Clock interface {
	Now() time.Time
}

type Reconciler struct {
	statements Statements
	books      Books
	cases      Cases
	calendar   *calendar.Calendar
	clock      Clock
}

func NewReconciler(statements Statements, books Books, cases Cases, cal *calendar.Calendar, clock Clock) *Reconciler {
	return &Reconciler{statements: statements, books: books, cases: cases, calendar: cal, clock: clock}
}

type Report struct {
	Divergences int
	// Settled went the way the provider's answer goes; Opened went to a person.
	Settled, Opened int
}

// Reconcile handles the divergences of day only, so each is handled by one day's run;
// a case already open for the same provider ID is not opened again.
func (r *Reconciler) Reconcile(ctx context.Context, provider string, day calendar.Date) (Report, error) {
	first := day.AddDays(1 - Window)
	var theirs []domain.Line
	for d := first; !d.After(day); d = d.AddDays(1) {
		lines, err := r.statements.Statement(ctx, provider, d)
		if err != nil {
			return Report{}, fmt.Errorf("reconciliation: %s statement of %s: %w", provider, d, err)
		}
		theirs = append(theirs, lines...)
	}
	loc := r.calendar.Location()
	since, until := first.StartIn(loc), day.EndIn(loc)
	ours, err := r.books.Movements(ctx, provider, since, until)
	if err != nil {
		return Report{}, fmt.Errorf("reconciliation: %s books: %w", provider, err)
	}
	held, err := r.cases.Held(ctx, provider, since, until)
	if err != nil {
		return Report{}, fmt.Errorf("reconciliation: %s held: %w", provider, err)
	}

	var rep Report
	now := r.clock.Now()
	for _, d := range domain.Compare(theirs, append(ours, held...)) {
		if !r.of(day, &d) {
			continue
		}
		rep.Divergences++
		if d.Kind == domain.MissingInSystem {
			settled, err := r.books.Settle(ctx, provider, d.Provider, now)
			if err != nil {
				return rep, fmt.Errorf("reconciliation: settle: %w", err)
			}
			if settled {
				rep.Settled++
				continue
			}
		}
		opened, err := r.cases.Open(ctx, caseOf(provider, &d, day, now))
		if err != nil {
			return rep, fmt.Errorf("reconciliation: open case: %w", err)
		}
		if opened {
			rep.Opened++
		}
	}
	return rep, nil
}

func (r *Reconciler) of(day calendar.Date, d *domain.Divergence) bool {
	for _, l := range []*domain.Line{d.Provider, d.Ours} {
		if l != nil && r.calendar.Today(l.At) == day {
			return true
		}
	}
	return false
}

func caseOf(provider string, d *domain.Divergence, day calendar.Date, now time.Time) *domain.Case {
	l := d.Provider
	var details string
	switch d.Kind {
	case domain.MissingInSystem:
		details = fmt.Sprintf("%s's statement of %s shows %s %s, which the gateway has not recorded", provider, day, l.Kind, l.Amount)
	case domain.MissingAtProvider:
		l = d.Ours
		details = fmt.Sprintf("the gateway recorded %s %s, which %s's statements up to %s do not show", l.Kind, l.Amount, provider, day)
	case domain.AmountDiffers:
		details = fmt.Sprintf("%s %s: %s's statement shows %s, the gateway recorded %s", l.Kind, day, provider, l.Amount, d.Ours.Amount)
	}
	external := l.ExternalID
	if external == "" {
		external = l.Key
	}
	return &domain.Case{
		Kind: domain.CaseKind(d.Kind), Provider: provider, ExternalID: external, ProviderKey: l.Key,
		Amount: l.Amount, Details: details, CreatedAt: now,
	}
}
