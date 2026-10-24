package app

import (
	"context"
	"fmt"
	"time"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

type Quoter interface {
	Quote(ctx context.Context, amount money.Money, payer money.Currency, validFor time.Duration) (fx.Quote, error)
}

type Store interface {
	// Create stores the quote and the pending payment together, or neither.
	Create(ctx context.Context, d *domain.Draft, q *fx.Quote) (domain.Payment, error)
}

type Service struct {
	quoter Quoter
	store  Store
}

func NewService(quoter Quoter, store Store) *Service {
	return &Service{quoter: quoter, store: store}
}

// Create prices the payment in BRL and stores it as pending (section 4.1, steps 1-2).
// The payer amount comes from the quote; the provider attempt arrives in F5.
func (s *Service) Create(ctx context.Context, merchant domain.MerchantID, method string, amount money.Money, reference string) (domain.Payment, fx.Quote, error) {
	draft, err := domain.NewDraft(merchant, method, amount, reference)
	if err != nil {
		return domain.Payment{}, fx.Quote{}, fmt.Errorf("payments: create: %w", err)
	}
	quote, err := s.quoter.Quote(ctx, amount, money.BRL, domain.PixQuoteValidity)
	if err != nil {
		return domain.Payment{}, fx.Quote{}, fmt.Errorf("payments: create: %w", err)
	}
	payment, err := s.store.Create(ctx, &draft, &quote)
	if err != nil {
		return domain.Payment{}, fx.Quote{}, fmt.Errorf("payments: create: %w", err)
	}
	return payment, quote, nil
}
