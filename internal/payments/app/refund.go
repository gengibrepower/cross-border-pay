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

var (
	// ErrPaymentNotRefundable is a refund of a payment that is not paid, or already
	// refunded in full.
	ErrPaymentNotRefundable = errors.New("payments: only a paid payment can be refunded")
	ErrRefundNotFound       = errors.New("payments: refund not found")
)

// RefundRequested is the recovery point of a refund whose row and hold are stored: a
// resumed request carries on from the refund's status instead of creating another.
const RefundRequested idempotency.RecoveryPoint = "refund_requested"

type RateSource interface {
	Rate(ctx context.Context, base, quote money.Currency) (fx.Rate, error)
}

type RefundStore interface {
	// CreateRefund locks the merchant's payment, prices the refund against the ones
	// that still count, and stores it in created with its hold posted and the
	// RefundRequested recovery point, or none of them.
	CreateRefund(ctx context.Context, held *idempotency.Held, merchant domain.MerchantID, payment domain.PaymentID, amount money.Money, provider domain.ProviderName, now time.Time) (domain.Refund, error)
	// GetRefund returns ErrRefundNotFound unless the merchant owns the refund's payment.
	GetRefund(ctx context.Context, merchant domain.MerchantID, id domain.RefundID) (domain.Refund, error)
	// PaidAttempt is the attempt that collected the payment, whose charge a refund
	// returns money from.
	PaidAttempt(ctx context.Context, payment domain.PaymentID) (domain.PaymentAttempt, error)
	// ApplyRefund moves the refund by u and posts what its new status means: the money
	// gone back, bought back at rate, with the payment's new status and
	// refund.succeeded; or the hold released with refund.failed. changed is false for a
	// duplicate.
	ApplyRefund(ctx context.Context, id domain.RefundID, u *domain.RefundUpdate, rate fx.Rate, now time.Time) (domain.Refund, bool, error)
}

// Refunder returns money of paid payments to their payers (section 4.7), with the same
// double-write rules as attempts (section 4.3).
type Refunder struct {
	rates           RateSource
	store           RefundStore
	provider        Provider
	clock           Clock
	logger          *slog.Logger
	providerTimeout time.Duration
}

func NewRefunder(rates RateSource, store RefundStore, provider Provider, clock Clock, logger *slog.Logger, providerTimeout time.Duration) *Refunder {
	return &Refunder{rates: rates, store: store, provider: provider, clock: clock, logger: logger, providerTimeout: providerTimeout}
}

// Refund refunds amount of the payment under the idempotency key held. The rate that
// prices the money gone back is today's, taken first: without it nothing is stored and
// the provider is not called.
func (r *Refunder) Refund(ctx context.Context, held *idempotency.Held, merchant domain.MerchantID, payment domain.PaymentID, amount money.Money) (domain.Refund, error) {
	rate, err := r.rates.Rate(ctx, domain.ChargeCurrency, money.BRL)
	if err != nil {
		return domain.Refund{}, fmt.Errorf("payments: refund: %w", err)
	}
	var refund domain.Refund
	switch held.RecoveryPoint {
	case idempotency.Started:
		refund, err = r.store.CreateRefund(ctx, held, merchant, payment, amount, r.provider.Name(), r.clock.Now())
	case RefundRequested:
		refund, err = r.store.GetRefund(ctx, merchant, held.ResourceID)
	default:
		err = fmt.Errorf("%w: %q", ErrRecoveryPoint, held.RecoveryPoint)
	}
	if err != nil {
		return domain.Refund{}, fmt.Errorf("payments: refund: %w", err)
	}
	// From here the provider may act on our behalf, so a client that gives up must not
	// cut the call or the write of its answer short (section 4.3).
	refund, err = r.send(context.WithoutCancel(ctx), &refund, rate)
	if err != nil {
		return domain.Refund{}, fmt.Errorf("payments: refund %s: %w", refund.ID(), err)
	}
	return refund, nil
}

// send is request for refunds: created means the provider was certainly not called,
// sent that it may have been, anything else that the answer is stored.
func (r *Refunder) send(ctx context.Context, refund *domain.Refund, rate fx.Rate) (domain.Refund, error) {
	switch refund.Status() {
	case domain.RefundCreated:
	case domain.RefundSent:
		return r.apply(ctx, refund, &domain.RefundUpdate{Event: domain.RefundAmbiguous}, rate)
	case domain.RefundUnknown, domain.RefundSucceeded, domain.RefundRejected:
		return *refund, nil
	default:
		return domain.Refund{}, fmt.Errorf("refund has unknown status %q", refund.Status())
	}

	attempt, err := r.store.PaidAttempt(ctx, refund.Payment())
	if err != nil {
		return domain.Refund{}, fmt.Errorf("paid attempt: %w", err)
	}
	sent, changed, err := r.store.ApplyRefund(ctx, refund.ID(), &domain.RefundUpdate{Event: domain.SendRefund}, rate, r.clock.Now())
	if err != nil {
		return domain.Refund{}, fmt.Errorf("apply %s: %w", domain.SendRefund, err)
	}
	if !changed {
		// A retried request and the resolver can both find the refund created; only the
		// one that moved it to sent calls the provider.
		return sent, nil
	}
	u := r.call(ctx, &sent, attempt.ExternalID())
	return r.apply(ctx, &sent, &u, rate)
}

// call asks the provider for the refund. Any error is ambiguous, never a refusal.
func (r *Refunder) call(ctx context.Context, refund *domain.Refund, charge string) domain.RefundUpdate {
	ctx, cancel := context.WithTimeout(ctx, r.providerTimeout)
	defer cancel()
	amounts := refund.Amounts()
	result, err := r.provider.Refund(ctx, &RefundRequest{Refund: refund.ID(), Charge: charge, Amount: amounts.PayerAmount})
	if err != nil {
		r.logger.WarnContext(ctx, "provider answer is ambiguous; refund goes to unknown",
			"refund", refund.ID().String(), "provider", string(refund.Provider()), "error", err)
		return domain.RefundUpdate{Event: domain.RefundAmbiguous}
	}
	return result.Update()
}

func (r *Refunder) apply(ctx context.Context, refund *domain.Refund, u *domain.RefundUpdate, rate fx.Rate) (domain.Refund, error) {
	next, _, err := r.store.ApplyRefund(ctx, refund.ID(), u, rate, r.clock.Now())
	if err != nil {
		return domain.Refund{}, fmt.Errorf("apply %s: %w", u.Event, err)
	}
	return next, nil
}

// Update is the result as a change to the refund. An outcome the adapter left unset is
// ambiguous, not a refusal.
func (r *RefundResult) Update() domain.RefundUpdate {
	switch r.Outcome {
	case RefundSucceeded:
		return domain.RefundUpdate{Event: domain.SucceedRefund, ExternalID: r.ExternalID}
	case RefundRejected:
		return domain.RefundUpdate{Event: domain.RejectRefund, ExternalID: r.ExternalID, Reason: r.Reason}
	default:
		return domain.RefundUpdate{Event: domain.RefundAmbiguous}
	}
}
