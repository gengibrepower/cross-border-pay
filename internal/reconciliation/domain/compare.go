package domain

import (
	"cmp"
	"slices"
	"strings"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

// MovementKind is money into or out of the gateway's account at a provider, with the
// values of the simulators' statements.
type MovementKind string

const (
	Received    MovementKind = "received"
	Refunded    MovementKind = "refunded"
	ChargedBack MovementKind = "charged_back"
	Reversed    MovementKind = "reversed"
)

// Line is one movement, from either side. Key is the gateway's own ID for it (attempt
// or refund); a chargeback has none and is known by ExternalID.
type Line struct {
	Kind       MovementKind
	ExternalID string
	Key        string
	Amount     money.Money
}

func (l *Line) identity() string {
	id := l.Key
	if id == "" {
		id = "external:" + l.ExternalID
	}
	return string(l.Kind) + "\x00" + id
}

// DivergenceKind is a row of section 4.8. A status that differs shows as
// MissingInSystem with a Key: whoever handles it finds the attempt or refund by it.
type DivergenceKind string

const (
	MissingInSystem   DivergenceKind = "missing_in_system"
	MissingAtProvider DivergenceKind = "missing_at_provider"
	AmountDiffers     DivergenceKind = "amount_differs"
)

// Divergence carries the line of each side that has one.
type Divergence struct {
	Kind     DivergenceKind
	Provider *Line
	Ours     *Line
}

// Compare knows nothing of where each side came from (decision 30). A line repeated on
// one side is missing on the other as many times, so a double posting is found too.
// The pairs do not depend on the order of either side: a line pairs with one of the
// same amount when there is one.
func Compare(provider, ours []Line) []Divergence {
	provider, ours = sorted(provider), sorted(ours)
	pending := map[string][]Line{}
	for _, l := range ours {
		pending[l.identity()] = append(pending[l.identity()], l)
	}
	var out []Divergence
	for _, p := range provider {
		id := p.identity()
		matches := pending[id]
		if len(matches) == 0 {
			out = append(out, Divergence{Kind: MissingInSystem, Provider: &p})
			continue
		}
		i := max(0, slices.IndexFunc(matches, func(o Line) bool { return o.Amount == p.Amount }))
		o := matches[i]
		pending[id] = slices.Delete(matches, i, i+1)
		if o.Amount != p.Amount {
			out = append(out, Divergence{Kind: AmountDiffers, Provider: &p, Ours: &o})
		}
	}
	for _, rest := range pending {
		for _, o := range rest {
			out = append(out, Divergence{Kind: MissingAtProvider, Ours: &o})
		}
	}
	slices.SortStableFunc(out, func(a, b Divergence) int {
		return cmp.Or(strings.Compare(string(a.Kind), string(b.Kind)), compareLines(a.line(), b.line()))
	})
	return out
}

func sorted(lines []Line) []Line {
	out := slices.Clone(lines)
	slices.SortFunc(out, func(a, b Line) int { return compareLines(&a, &b) })
	return out
}

func compareLines(a, b *Line) int {
	return cmp.Or(strings.Compare(a.identity(), b.identity()), strings.Compare(string(a.Kind), string(b.Kind)),
		strings.Compare(a.Amount.Currency().Code(), b.Amount.Currency().Code()),
		cmp.Compare(a.Amount.Amount(), b.Amount.Amount()),
		strings.Compare(a.ExternalID, b.ExternalID))
}

func (d *Divergence) line() *Line {
	if d.Provider != nil {
		return d.Provider
	}
	return d.Ours
}
