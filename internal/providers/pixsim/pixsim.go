// Package pixsim is the client of the PIX simulator in cmd/pixsim, a provider that
// fails on command. It speaks only the simulator's own types; payments translates them.
package pixsim

import (
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

type CreateCharge struct {
	Amount    money.Money `json:"amount"`
	ExpiresAt time.Time   `json:"expires_at"`
}

type Status string

const (
	Active   Status = "active"
	Paid     Status = "paid"
	Rejected Status = "rejected"
)

type Charge struct {
	ID             string      `json:"id"`
	IdempotencyKey string      `json:"idempotency_key"`
	Status         Status      `json:"status"`
	Amount         money.Money `json:"amount"`
	// TxID and BRCode are empty on a rejected charge.
	TxID      string     `json:"txid,omitempty"`
	BRCode    string     `json:"br_code,omitempty"`
	ExpiresAt time.Time  `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
	PaidAt    *time.Time `json:"paid_at,omitempty"`
}

type Op string

const (
	OpCreate Op = "create"
	OpGet    Op = "get"
)

type FaultKind string

const (
	// Reject refuses the create for good, as a provider that says no.
	Reject FaultKind = "reject"
	// FailBefore answers 500 without doing anything.
	FailBefore FaultKind = "fail_before"
	// TimeoutAfter does the work and then never answers: the ambiguous case of
	// section 4.3, where the provider acted and the caller cannot know it.
	TimeoutAfter FaultKind = "timeout_after"
	// Delay waits DelayMS before doing the work and answering normally.
	Delay FaultKind = "delay"
)

// Fault makes the next Count requests of Op fail as Kind, in the order the faults
// were added.
type Fault struct {
	Op      Op        `json:"op"`
	Kind    FaultKind `json:"fault"`
	Count   int       `json:"count"`
	DelayMS int64     `json:"delay_ms,omitempty"`
}

// Problem codes the simulator answers with.
const (
	CodeRejected    = "charge_rejected"
	CodeKeyReused   = "idempotency_key_reused"
	CodeNotFound    = "charge_not_found"
	CodeInvalid     = "invalid_request"
	CodeNotPayable  = "charge_not_payable"
	CodeFailedFault = "internal_error"
)
