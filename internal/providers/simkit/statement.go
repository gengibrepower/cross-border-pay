package simkit

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

// OpStatement is never refused: a statement only reports.
const OpStatement Op = "statement"

// MovementKind is money into or out of the gateway's account at the provider.
type MovementKind string

const (
	Received    MovementKind = "received"
	Refunded    MovementKind = "refunded"
	ChargedBack MovementKind = "charged_back"
	// Reversed is a won chargeback: the money comes back.
	Reversed MovementKind = "reversed"
)

// Movement's IdempotencyKey is the gateway's own ID for it (attempt or refund), so the
// gateway finds its record; a chargeback has none and names what it disputes in
// ParentID.
type Movement struct {
	Kind           MovementKind `json:"kind"`
	ID             string       `json:"id"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
	ParentID       string       `json:"parent_id,omitempty"`
	Amount         money.Money  `json:"amount"`
	At             time.Time    `json:"at"`
}

type Statement struct {
	Date      calendar.Date `json:"date"`
	Movements []Movement    `json:"movements"`
}

// The day of a statement is the bank's, in São Paulo, as the settlement's cutoff.
var statementZone = sync.OnceValues(func() (*time.Location, error) {
	cal, err := calendar.Brazil()
	return cal.Location(), err
})

// StatementOf keeps the movements of date in loc, in an order that depends only on what
// they are, so the same seed gives the same statement byte for byte.
func StatementOf(date calendar.Date, loc *time.Location, all []Movement) Statement {
	from, to := date.StartIn(loc), date.EndIn(loc)
	out := []Movement{}
	for _, m := range all {
		if !m.At.Before(from) && m.At.Before(to) {
			m.At = m.At.UTC()
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b Movement) int {
		return cmp.Or(a.At.Compare(b.At), strings.Compare(a.ID, b.ID), strings.Compare(string(a.Kind), string(b.Kind)))
	})
	return Statement{Date: date, Movements: out}
}

// ServeStatement answers GET /statements/{date} with the day's part of movements,
// which a simulator reads under its own lock.
func ServeStatement(faults *Faults, logger *slog.Logger, movements func() []Movement) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		date, err := calendar.ParseDate(r.PathValue("date"))
		if err != nil {
			Invalid(w, r, "date must be YYYY-MM-DD")
			return
		}
		fault := faults.Next(OpStatement)
		if !Before(w, r, &fault) {
			return
		}
		After(r, &fault)
		loc, err := statementZone()
		if err != nil {
			httpapi.WriteInternalError(w, r, logger, err)
			return
		}
		WriteJSON(w, http.StatusOK, StatementOf(date, loc, movements()))
	}
}

func (c *Client) Statement(ctx context.Context, date calendar.Date) (Statement, error) {
	var s Statement
	err := c.Do(ctx, http.MethodGet, "/statements/"+date.String(), "", nil, http.StatusOK, &s)
	return s, err
}
