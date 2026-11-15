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

// Section 4.8, from the daily reconciliation: the values of DivergenceKind.
const (
	MissingInSystemCase   = CaseKind(MissingInSystem)
	MissingAtProviderCase = CaseKind(MissingAtProvider)
	AmountDiffersCase     = CaseKind(AmountDiffers)
)

// Case's Payment and Attempt are nil for money that cannot be tied to one, such as
// what the reconciliation finds. LedgerReference is empty for a case without a posting.
type Case struct {
	Kind       CaseKind
	Provider   string
	ExternalID string
	// ProviderKey is the key the provider named, kept when no attempt has it.
	ProviderKey     string
	Payment         *[16]byte
	Attempt         *[16]byte
	Amount          money.Money
	LedgerReference string
	Details         string
	CreatedAt       time.Time
}
