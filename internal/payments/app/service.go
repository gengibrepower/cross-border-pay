package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

type Quoter interface {
	Quote(ctx context.Context, amount money.Money, payer money.Currency, validFor time.Duration) (fx.Quote, error)
}

var (
	// ErrPaymentNotFound also covers another merchant's payment, so a merchant cannot
	// learn that it exists.
	ErrPaymentNotFound = errors.New("payments: payment not found")
	ErrInvalidLimit    = errors.New("payments: limit out of range")
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Priced is a payment with the quote that priced it.
type Priced struct {
	Payment domain.Payment
	Quote   fx.Quote
}

type ListQuery struct {
	// Status filters the list; the zero value lists every status.
	Status domain.Status
	// Before lists payments older than this one; the zero value starts at the newest.
	Before domain.PaymentID
	Limit  int
}

// Page is newest first. More means there are older payments past the last one.
type Page struct {
	Payments []Priced
	More     bool
}

type Store interface {
	// Create stores the quote and the pending payment together, or neither.
	Create(ctx context.Context, d *domain.Draft, q *fx.Quote) (domain.Payment, error)
	// Get returns ErrPaymentNotFound unless the merchant owns the payment.
	Get(ctx context.Context, merchant domain.MerchantID, id domain.PaymentID) (Priced, error)
	// List returns at most q.Limit payments of the merchant, newest first.
	List(ctx context.Context, merchant domain.MerchantID, q ListQuery) ([]Priced, error)
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

func (s *Service) Get(ctx context.Context, merchant domain.MerchantID, id domain.PaymentID) (Priced, error) {
	p, err := s.store.Get(ctx, merchant, id)
	if err != nil {
		return Priced{}, fmt.Errorf("payments: get: %w", err)
	}
	return p, nil
}

func (s *Service) List(ctx context.Context, merchant domain.MerchantID, q ListQuery) (Page, error) {
	if q.Limit < 1 || q.Limit > MaxPageSize {
		return Page{}, fmt.Errorf("payments: list: %w: %d is not in 1..%d", ErrInvalidLimit, q.Limit, MaxPageSize)
	}
	// One row past the page tells whether another page exists, without a count.
	want := q.Limit
	q.Limit++
	payments, err := s.store.List(ctx, merchant, q)
	if err != nil {
		return Page{}, fmt.Errorf("payments: list: %w", err)
	}
	if len(payments) > want {
		return Page{Payments: payments[:want], More: true}, nil
	}
	return Page{Payments: payments}, nil
}
