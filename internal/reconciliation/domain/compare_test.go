package domain_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/reconciliation/domain"
)

func describe(ds []domain.Divergence) []string {
	line := func(l *domain.Line) string {
		if l == nil {
			return "-"
		}
		return fmt.Sprintf("%s/%s/%s/%s", l.Kind, l.Key, l.ExternalID, l.Amount)
	}
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = string(d.Kind) + " " + line(d.Provider) + " " + line(d.Ours)
	}
	slices.Sort(out)
	return out
}

// Every difference planted becomes exactly its divergence, whatever the order of
// either side, and lines that agree yield nothing.
func TestCompareFindsExactlyWhatDiffers(t *testing.T) {
	kinds := []domain.MovementKind{domain.Received, domain.Refunded, domain.ChargedBack, domain.Reversed}
	rapid.Check(t, func(t *rapid.T) {
		var provider, ours []domain.Line
		var want []domain.Divergence
		n := rapid.IntRange(0, 40).Draw(t, "lines")
		for i := range n {
			p := domain.Line{
				Kind:       rapid.SampledFrom(kinds).Draw(t, "kind"),
				ExternalID: fmt.Sprintf("ext_%d", i),
				Amount:     money.New(rapid.Int64Range(1, 1_000_000).Draw(t, "amount"), money.BRL),
			}
			if p.Kind == domain.Received || p.Kind == domain.Refunded {
				p.Key = fmt.Sprintf("key_%d", i)
			}
			o := p
			// The gateway may not know the provider's ID of something it sent, yet.
			if o.Key != "" && rapid.Bool().Draw(t, "external id unknown") {
				o.ExternalID = ""
			}
			switch rapid.IntRange(0, 5).Draw(t, "plant") {
			case 0:
				provider, ours = append(provider, p), append(ours, o)
			case 1:
				provider = append(provider, p)
				want = append(want, domain.Divergence{Kind: domain.MissingInSystem, Provider: &p})
			case 2:
				ours = append(ours, o)
				want = append(want, domain.Divergence{Kind: domain.MissingAtProvider, Ours: &o})
			case 3:
				o.Amount = money.New(p.Amount.Amount()+rapid.Int64Range(1, 100).Draw(t, "delta"), money.BRL)
				if rapid.Bool().Draw(t, "another currency") {
					o.Amount = money.New(p.Amount.Amount(), money.USD)
				}
				provider, ours = append(provider, p), append(ours, o)
				want = append(want, domain.Divergence{Kind: domain.AmountDiffers, Provider: &p, Ours: &o})
			case 4:
				provider, ours = append(provider, p), append(ours, o, o)
				want = append(want, domain.Divergence{Kind: domain.MissingAtProvider, Ours: &o})
			case 5:
				provider, ours = append(provider, p, p), append(ours, o)
				want = append(want, domain.Divergence{Kind: domain.MissingInSystem, Provider: &p})
			}
		}
		got := domain.Compare(provider, ours)
		if g, w := describe(got), describe(want); !reflect.DeepEqual(g, w) {
			t.Fatalf("Compare = %v\nwant %v", g, w)
		}
		shuffled := domain.Compare(rapid.Permutation(provider).Draw(t, "provider order"), rapid.Permutation(ours).Draw(t, "our order"))
		if !reflect.DeepEqual(shuffled, got) {
			t.Fatalf("the order of the sides changed the result:\n%v\n%v", describe(shuffled), describe(got))
		}
	})
}

func TestComparePairs(t *testing.T) {
	line := func(kind domain.MovementKind, key, external string, amount int64) domain.Line {
		return domain.Line{Kind: kind, Key: key, ExternalID: external, Amount: money.New(amount, money.BRL)}
	}
	paid := func(amount int64) domain.Line { return line(domain.Received, "att_1", "chg_1", amount) }
	tests := []struct {
		name           string
		provider, ours []domain.Line
		want           []string
	}{
		{
			// A double posting with one wrong copy is one surplus line, not an amount
			// difference and a surplus.
			"the same amount first",
			[]domain.Line{paid(100)},
			[]domain.Line{paid(50), paid(100)},
			[]string{"missing_at_provider - received/att_1/chg_1/0.50 BRL"},
		},
		{
			"without one, by amount, whatever the order",
			[]domain.Line{paid(100)},
			[]domain.Line{paid(70), paid(50)},
			[]string{"amount_differs received/att_1/chg_1/1.00 BRL received/att_1/chg_1/0.50 BRL", "missing_at_provider - received/att_1/chg_1/0.70 BRL"},
		},
		{
			"a chargeback and its reversal share the provider's ID",
			[]domain.Line{line(domain.ChargedBack, "", "cbk_1", 3000), line(domain.Reversed, "", "cbk_1", 3000)},
			[]domain.Line{line(domain.Reversed, "", "cbk_1", 3000)},
			[]string{"missing_in_system charged_back//cbk_1/30.00 BRL -"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reversed := slices.Clone(tt.ours)
			slices.Reverse(reversed)
			for _, ours := range [][]domain.Line{tt.ours, reversed} {
				if got := describe(domain.Compare(tt.provider, ours)); !reflect.DeepEqual(got, tt.want) {
					t.Errorf("Compare(%v) = %v, want %v", ours, got, tt.want)
				}
			}
		})
	}
}
