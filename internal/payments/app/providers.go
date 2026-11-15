package app

import (
	"errors"
	"fmt"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// ErrUnknownProvider means the deploy dropped a provider that still holds money.
var ErrUnknownProvider = errors.New("payments: unknown provider")

// Providers routes a new payment by its method, and anything after that by the name
// stored with the attempt: a refund goes where the money was paid, whatever the routes
// say now.
type Providers struct {
	byMethod map[domain.Method]Provider
	byName   map[domain.ProviderName]Provider
}

// NewProviders refuses two providers with the same name: stored attempts could not
// tell them apart.
func NewProviders(routes map[domain.Method]Provider) (*Providers, error) {
	p := &Providers{byMethod: make(map[domain.Method]Provider), byName: make(map[domain.ProviderName]Provider)}
	for m, prov := range routes {
		if _, err := domain.ParseMethod(string(m)); err != nil {
			return nil, fmt.Errorf("payments: route: %w", err)
		}
		if seen, ok := p.byName[prov.Name()]; ok && seen != prov {
			return nil, fmt.Errorf("payments: two providers named %q", prov.Name())
		}
		p.byMethod[m] = prov
		p.byName[prov.Name()] = prov
	}
	return p, nil
}

func (p *Providers) For(m domain.Method) (Provider, error) {
	prov, ok := p.byMethod[m]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrUnsupportedMethod, m)
	}
	return prov, nil
}

func (p *Providers) Named(n domain.ProviderName) (Provider, error) {
	prov, ok := p.byName[n]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, n)
	}
	return prov, nil
}
