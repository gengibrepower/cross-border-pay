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
	// UpdateEndpoint locks the merchant, hands its endpoint to change (nil for none) and
	// stores what change returns, in one transaction that it may repeat whole.
	UpdateEndpoint(ctx context.Context, merchant domain.MerchantID, change func(current *domain.Endpoint) (domain.Endpoint, error)) (domain.Endpoint, error)
	// Endpoint returns ErrEndpointNotFound for a merchant without one.
	Endpoint(ctx context.Context, merchant domain.MerchantID) (domain.Endpoint, error)
}

// Endpoints registers where merchants receive webhooks.
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

// Get returns the merchant's endpoint, disabled or not, or ErrEndpointNotFound. It is
// how a merchant learns that deliveries stopped (decision 24).
func (s *Endpoints) Get(ctx context.Context, merchant domain.MerchantID) (domain.Endpoint, error) {
	e, err := s.store.Endpoint(ctx, merchant)
	if err != nil {
		return domain.Endpoint{}, fmt.Errorf("merchants: webhook endpoint: %w", err)
	}
	return e, nil
}

// Put registers or changes the merchant's endpoint, with times in the database's
// microseconds. secret is empty unless the change shows it (domain.Apply). A URL the
// policy refuses is ssrf.ErrInvalidURL or ssrf.ErrForbiddenURL.
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
