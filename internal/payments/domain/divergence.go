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
	// ErrAmountMismatch is a payment the provider confirmed for another amount than the
	// quote's: real money, but not this payment's.
	ErrAmountMismatch = errors.New("payments: paid amount differs from the quote")
	// ErrAlreadyPaid is a second attempt paid for a payment another one already paid.
	ErrAlreadyPaid = errors.New("payments: payment already paid")
	// ErrPaidAfterRejection is money for an attempt the provider had rejected.
	ErrPaidAfterRejection = errors.New("payments: attempt paid after being rejected")
)

// CheckPaid is the one rule every path that learns of a payment goes through, whether
// the provider's event, the resolver or the expiry job: the amount the quote asked for,
// paid on time.
func CheckPaid(q *fx.Quote, amount money.Money, paidAt time.Time) error {
	if amount != q.PayerAmount() {
		return fmt.Errorf("%w: paid %s, quoted %s", ErrAmountMismatch, amount, q.PayerAmount())
	}
	return CheckOnTime(q, paidAt)
}

// ConfirmPayment is the payment's side of an attempt that just succeeded: the status it
// moves to, or why the money confirms nothing, which DivergenceOf names.
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
)

// DivergenceOf names the divergence an error is, for money the provider confirmed. Any
// other error is not one: the money cannot be posted and the event is not settled.
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

// ReviewCase is what a person gets to decide where the money goes (decision 8, F7).
type ReviewCase struct {
	Kind            DivergenceKind
	Provider        ProviderName
	ExternalID      string
	Payment         PaymentID
	Attempt         AttemptID
	Amount          money.Money
	LedgerReference string
	Details         string
	CreatedAt       time.Time
}

// DivergenceReference names one provider charge in the ledger, so it is posted once
// however many paths report it.
func DivergenceReference(provider ProviderName, externalID string) string {
	return "divergence:" + string(provider) + ":" + externalID
}

// DivergencePosting takes money in at the provider and holds it on the divergence
// account until a person decides where it goes: confirmed money always enters the ledger.
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
