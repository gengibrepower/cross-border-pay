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

// Case is money a provider confirmed that confirmed nothing. Payment and Attempt are
// nil for money that cannot be tied to one, such as the reconciliation's in F9.
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
