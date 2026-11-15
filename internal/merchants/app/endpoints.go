package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/ssrf"
)

var (
	ErrMerchantNotFound = errors.New("merchants: merchant not found")
	ErrEndpointNotFound = errors.New("merchants: no webhook endpoint")
)

type Clock interface {
	Now() time.Time
}

type EndpointStore interface {
	// UpdateEndpoint may repeat its transaction whole, change included.
	UpdateEndpoint(ctx context.Context, merchant domain.MerchantID, change func(current *domain.Endpoint) (domain.Endpoint, error)) (domain.Endpoint, error)
	Endpoint(ctx context.Context, merchant domain.MerchantID) (domain.Endpoint, error)
}

type Endpoints struct {
	store  EndpointStore
	policy ssrf.Policy
	key    []byte
	clock  Clock
}

func NewEndpoints(store EndpointStore, policy ssrf.Policy, signingKey []byte, clock Clock) (*Endpoints, error) {
	if len(signingKey) < domain.MinSigningKeyBytes {
		return nil, domain.ErrWeakSigningKey
	}
	return &Endpoints{store: store, policy: policy, key: signingKey, clock: clock}, nil
}

// Get is how a merchant learns that deliveries stopped (decision 24).
func (s *Endpoints) Get(ctx context.Context, merchant domain.MerchantID) (domain.Endpoint, error) {
	e, err := s.store.Endpoint(ctx, merchant)
	if err != nil {
		return domain.Endpoint{}, fmt.Errorf("merchants: webhook endpoint: %w", err)
	}
	return e, nil
}

// Put keeps times in the database's microseconds.
func (s *Endpoints) Put(ctx context.Context, merchant domain.MerchantID, c domain.Change) (e domain.Endpoint, secret string, err error) {
	if _, err := s.policy.ValidateURL(c.URL); err != nil {
		return domain.Endpoint{}, "", fmt.Errorf("merchants: %w", err)
	}
	var shown bool
	e, err = s.store.UpdateEndpoint(ctx, merchant, func(current *domain.Endpoint) (domain.Endpoint, error) {
		next, show, err := domain.Apply(current, merchant, c, s.clock.Now().Truncate(time.Microsecond))
		if err != nil {
			return domain.Endpoint{}, fmt.Errorf("change: %w", err)
		}
		shown = show
		return next, nil
	})
	if err != nil {
		return domain.Endpoint{}, "", fmt.Errorf("merchants: webhook endpoint: %w", err)
	}
	if !shown {
		return e, "", nil
	}
	secret, err = domain.DeriveSecret(s.key, merchant, e.Version)
	if err != nil {
		return domain.Endpoint{}, "", fmt.Errorf("merchants: webhook secret: %w", err)
	}
	return e, secret, nil
}
