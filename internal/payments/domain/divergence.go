package domain

import (
	"errors"
	"fmt"
	"time"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

var (
	// ErrAmountMismatch is real money, but not this payment's.
	ErrAmountMismatch     = errors.New("payments: paid amount differs from the quote")
	ErrAlreadyPaid        = errors.New("payments: payment already paid")
	ErrPaidAfterRejection = errors.New("payments: attempt paid after being rejected")
)

// CheckPaid is the one rule every path that learns of a payment goes through: the
// provider's event, the resolver and the expiry job.
func CheckPaid(q *fx.Quote, amount money.Money, paidAt time.Time) error {
	if amount != q.PayerAmount() {
		return fmt.Errorf("%w: paid %s, quoted %s", ErrAmountMismatch, amount, q.PayerAmount())
	}
	return CheckOnTime(q, paidAt)
}

// ConfirmPayment's errors are named by DivergenceOf.
func ConfirmPayment(status Status, q *fx.Quote, amount money.Money, paidAt time.Time) (Status, error) {
	to, changed, err := Transition(status, Confirm)
	if err != nil {
		return status, err
	}
	if !changed {
		return status, fmt.Errorf("%w: payment is %s", ErrAlreadyPaid, status)
	}
	if err := CheckPaid(q, amount, paidAt); err != nil {
		return status, err
	}
	return to, nil
}

// DivergenceKind is why confirmed money confirmed nothing (section 4.4).
type DivergenceKind string

const (
	LatePayment        DivergenceKind = "late_payment"
	AmountMismatch     DivergenceKind = "amount_mismatch"
	PaidAfterRejection DivergenceKind = "paid_after_rejection"
	SecondCharge       DivergenceKind = "second_charge"
	// MissingInSystem is money a provider's statement shows for no attempt of ours.
	MissingInSystem DivergenceKind = "missing_in_system"
)

// DivergenceOf: any other error means the money cannot be posted and the event is not
// settled.
func DivergenceOf(err error) (DivergenceKind, bool) {
	switch {
	case errors.Is(err, ErrAmountMismatch):
		return AmountMismatch, true
	case errors.Is(err, ErrLatePayment):
		return LatePayment, true
	case errors.Is(err, ErrPaidAfterRejection):
		return PaidAfterRejection, true
	case errors.Is(err, ErrAlreadyPaid), errors.Is(err, ErrExternalIDMismatch):
		return SecondCharge, true
	default:
		return "", false
	}
}

// ReviewCase lets a person decide where the money goes (decision 8, F7).
type ReviewCase struct {
	Kind       DivergenceKind
	Provider   ProviderName
	ExternalID string
	// ProviderKey is the key the provider named when no attempt has it.
	ProviderKey     string
	Payment         PaymentID
	Attempt         AttemptID
	Amount          money.Money
	LedgerReference string
	Details         string
	CreatedAt       time.Time
}

// DivergenceReference is per provider charge, so it posts once however many paths
// report it.
func DivergenceReference(provider ProviderName, externalID string) string {
	return "divergence:" + string(provider) + ":" + externalID
}

// DivergencePosting: confirmed money always enters the ledger, held until a person
// decides.
func DivergencePosting(reference string, receivables, divergence ledger.AccountID, amount money.Money) (ledger.Transaction, error) {
	held, err := amount.Neg()
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("payments: divergence %s: %w", reference, err)
	}
	t, err := ledger.NewTransaction(reference, ledger.NewEntry(receivables, amount), ledger.NewEntry(divergence, held))
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("payments: divergence %s: %w", reference, err)
	}
	return t, nil
}
