package app_test

import (
	"errors"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// named is a provider that only has a name: routing never calls the others.
type named struct {
	app.Provider
	name domain.ProviderName
}

func (n *named) Name() domain.ProviderName { return n.name }

func TestProvidersRouteByMethodAndByName(t *testing.T) {
	pix, card := &named{name: "pixsim"}, &named{name: "cardsim"}
	p, err := app.NewProviders(map[domain.Method]app.Provider{domain.Pix: pix, domain.Card: card})
	if err != nil {
		t.Fatal(err)
	}
	for m, want := range map[domain.Method]app.Provider{domain.Pix: pix, domain.Card: card} {
		if got, err := p.For(m); err != nil || got != want {
			t.Errorf("For(%s) = %v, %v; want %v", m, got, err, want)
		}
	}
	if _, err := p.For(domain.Boleto); !errors.Is(err, domain.ErrUnsupportedMethod) {
		t.Errorf("For(boleto) error = %v, want %v", err, domain.ErrUnsupportedMethod)
	}
	if got, err := p.Named("cardsim"); err != nil || got != card {
		t.Errorf("Named(cardsim) = %v, %v; want the card provider", got, err)
	}
	if _, err := p.Named("efi"); !errors.Is(err, app.ErrUnknownProvider) {
		t.Errorf("Named(efi) error = %v, want %v", err, app.ErrUnknownProvider)
	}
}

// One provider may take several methods, but two providers may not share a name:
// stored attempts would not know which one holds them.
func TestProvidersRefuseTwoWithOneName(t *testing.T) {
	shared := &named{name: "bank"}
	if _, err := app.NewProviders(map[domain.Method]app.Provider{domain.Pix: shared, domain.Boleto: shared}); err != nil {
		t.Errorf("one provider for two methods: %v", err)
	}
	if _, err := app.NewProviders(map[domain.Method]app.Provider{
		domain.Pix: &named{name: "bank"}, domain.Boleto: &named{name: "bank"},
	}); err == nil {
		t.Error("two providers named bank were accepted")
	}
	if _, err := app.NewProviders(map[domain.Method]app.Provider{"crypto": shared}); !errors.Is(err, domain.ErrUnknownMethod) {
		t.Errorf("route for an unknown method error = %v, want %v", err, domain.ErrUnknownMethod)
	}
}
