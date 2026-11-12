package app

import (
	"context"
	"fmt"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// SettlementQuery pages through a merchant's settlements, newest cutoff first.
type SettlementQuery struct {
	// Before lists settlements of cutoff dates before this one; the zero value starts at
	// the newest.
	Before time.Time
	Limit  int
}

// SettlementPage is newest cutoff first. More means there are older settlements past the last.
type SettlementPage struct {
	Settlements []domain.Settlement
	More        bool
}

type PayoutStore interface {
	// Balance reads the merchant's payable and what of it is pending at now, at one
	// point in time.
	Balance(ctx context.Context, merchant domain.MerchantID, policy *domain.SettlementPolicy, now time.Time) (domain.Balance, error)
	// ListSettlements returns at most q.Limit settlements of the merchant, newest cutoff
	// first,
	// each with its payments.
	ListSettlements(ctx context.Context, merchant domain.MerchantID, q SettlementQuery) ([]domain.Settlement, error)
}

// Payouts answers what the merchant has with the platform and what it was paid
// (section 4.9), by the same policy the settlement job pays with.
type Payouts struct {
	store  PayoutStore
	policy domain.SettlementPolicy
	clock  Clock
}

func NewPayouts(store PayoutStore, policy domain.SettlementPolicy, clock Clock) *Payouts {
	return &Payouts{store: store, policy: policy, clock: clock}
}

func (p *Payouts) Balance(ctx context.Context, merchant domain.MerchantID) (domain.Balance, error) {
	b, err := p.store.Balance(ctx, merchant, &p.policy, p.clock.Now())
	if err != nil {
		return domain.Balance{}, fmt.Errorf("payments: balance: %w", err)
	}
	return b, nil
}

func (p *Payouts) Settlements(ctx context.Context, merchant domain.MerchantID, q SettlementQuery) (SettlementPage, error) {
	if q.Limit < 1 || q.Limit > MaxPageSize {
		return SettlementPage{}, fmt.Errorf("payments: settlements: %w: %d is not in 1..%d", ErrInvalidLimit, q.Limit, MaxPageSize)
	}
	want := q.Limit
	q.Limit++
	s, err := p.store.ListSettlements(ctx, merchant, q)
	if err != nil {
		return SettlementPage{}, fmt.Errorf("payments: settlements: %w", err)
	}
	if len(s) > want {
		return SettlementPage{Settlements: s[:want], More: true}, nil
	}
	return SettlementPage{Settlements: s}, nil
}
