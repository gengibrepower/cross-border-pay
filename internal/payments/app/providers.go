package app

import (
	"errors"
	"fmt"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// ErrUnknownProvider is an attempt or refund stored with a provider this process was
// not given: the deploy dropped a provider that still holds money.
var ErrUnknownProvider = errors.New("payments: unknown provider")

// Providers routes a new payment to the provider of its method, and an attempt or a
// refund back to the provider that holds it, by the name stored with it. Once a payment
// has an attempt, its method no longer picks the provider: a refund goes where the
// money was paid, whatever the routes say now.
type Providers struct {
	byMethod map[domain.Method]Provider
	byName   map[domain.ProviderName]Provider
}

// NewProviders takes each method's provider. A method left out is not offered, and a
// payment of it is domain.ErrUnsupportedMethod. Two providers with the same name are an
// error: stored attempts could not tell them apart.
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

// For returns the provider that takes new payments of the method.
func (p *Providers) For(m domain.Method) (Provider, error) {
	prov, ok := p.byMethod[m]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrUnsupportedMethod, m)
	}
	return prov, nil
}

// Named returns the provider stored with an attempt or a refund.
func (p *Providers) Named(n domain.ProviderName) (Provider, error) {
	prov, ok := p.byName[n]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, n)
	}
	return prov, nil
}
