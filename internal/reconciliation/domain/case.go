// Package domain holds the review queue of section 4.4: money and events the system
// could not settle on its own, waiting for a person.
package domain

import (
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

type CaseKind string

const (
	LatePayment        CaseKind = "late_payment"
	AmountMismatch     CaseKind = "amount_mismatch"
	PaidAfterRejection CaseKind = "paid_after_rejection"
	SecondCharge       CaseKind = "second_charge"
)

// Case's Payment and Attempt are nil for money that cannot be tied to one, such as
// what the reconciliation of F10 finds.
type Case struct {
	Kind            CaseKind
	Provider        string
	ExternalID      string
	Payment         *[16]byte
	Attempt         *[16]byte
	Amount          money.Money
	LedgerReference string
	Details         string
	CreatedAt       time.Time
}
