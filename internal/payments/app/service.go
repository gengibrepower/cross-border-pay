package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	idempotency "github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

type Quoter interface {
	Quote(ctx context.Context, amount money.Money, payer money.Currency, validFor time.Duration) (fx.Quote, error)
}

type Clock interface {
	Now() time.Time
}

var (
	// ErrPaymentNotFound also covers another merchant's payment, so a merchant cannot
	// learn that it exists.
	ErrPaymentNotFound = errors.New("payments: payment not found")
	ErrInvalidLimit    = errors.New("payments: limit out of range")
	ErrRecoveryPoint   = errors.New("payments: unknown recovery point")
)

// AttemptRequested is the recovery point of a create whose payment and attempt are
// stored: a resumed request carries on from the attempt's status instead of creating
// another payment.
const AttemptRequested idempotency.RecoveryPoint = "attempt_requested"

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Priced is a payment with the quote that priced it and its latest attempt, which is
// nil only for a payment stored before attempts existed.
type Priced struct {
	Payment domain.Payment
	Quote   fx.Quote
	Attempt *domain.PaymentAttempt
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
	// Create stores the quote, the pending payment, its attempt in created and the
	// AttemptRequested recovery point together, or none of them.
	Create(ctx context.Context, held *idempotency.Held, d *domain.Draft, q *fx.Quote, provider domain.ProviderName) (Priced, error)
	// Get returns ErrPaymentNotFound unless the merchant owns the payment.
	Get(ctx context.Context, merchant domain.MerchantID, id domain.PaymentID) (Priced, error)
	// List returns at most q.Limit payments of the merchant, newest first.
	List(ctx context.Context, merchant domain.MerchantID, q ListQuery) ([]Priced, error)
	ApplyAttempt(ctx context.Context, id domain.AttemptID, u *domain.AttemptUpdate, now time.Time) (domain.PaymentAttempt, bool, error)
}

type Service struct {
	quoter          Quoter
	store           Store
	provider        Provider
	clock           Clock
	logger          *slog.Logger
	providerTimeout time.Duration
}

func NewService(quoter Quoter, store Store, provider Provider, clock Clock, logger *slog.Logger, providerTimeout time.Duration) *Service {
	return &Service{quoter: quoter, store: store, provider: provider, clock: clock, logger: logger, providerTimeout: providerTimeout}
}

// Create prices the payment, stores it with its attempt and asks the provider for the
// PIX code (section 4.1, steps 1-3), under the idempotency key held. Each write leaves
// a status that tells a resumed request what is safe to do next (section 4.3):
//
//	created  the provider was certainly not called yet: call it
//	sent     it may have been called: mark unknown, never call again in the dark
//	other    the answer is stored: return it
func (s *Service) Create(ctx context.Context, held *idempotency.Held, merchant domain.MerchantID, method string, amount money.Money, reference string) (Priced, error) {
	var p Priced
	switch held.RecoveryPoint {
	case idempotency.Started:
		created, err := s.createPayment(ctx, held, merchant, method, amount, reference)
		if err != nil {
			return Priced{}, err
		}
		p = created
	case AttemptRequested:
		stored, err := s.store.Get(ctx, merchant, held.ResourceID)
		if err != nil {
			return Priced{}, fmt.Errorf("payments: resume create: %w", err)
		}
		if stored.Attempt == nil {
			return Priced{}, fmt.Errorf("payments: resume create: payment %s has no attempt", stored.Payment.ID())
		}
		p = stored
	default:
		return Priced{}, fmt.Errorf("%w: %q", ErrRecoveryPoint, held.RecoveryPoint)
	}

	// From here the provider may act on our behalf, so a client that gives up must
	// not cut the call or the write of its answer short (section 4.3).
	attempt, err := s.request(context.WithoutCancel(ctx), &p)
	if err != nil {
		return Priced{}, fmt.Errorf("payments: create: %w", err)
	}
	p.Attempt = &attempt
	return p, nil
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
