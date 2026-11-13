// Package pixsim is the client of the PIX simulator in cmd/pixsim, a provider that
// fails on command. It speaks only the simulator's own types; payments translates them.
package pixsim

import (
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
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

// CreateRefund returns part or all of a paid charge to the payer, as a PIX devolução.
type CreateRefund struct {
	Amount money.Money `json:"amount"`
}

type RefundStatus string

const (
	RefundCompleted RefundStatus = "completed"
	RefundRejected  RefundStatus = "rejected"
)

type Refund struct {
	ID             string       `json:"id"`
	IdempotencyKey string       `json:"idempotency_key"`
	ChargeID       string       `json:"charge_id"`
	Status         RefundStatus `json:"status"`
	Amount         money.Money  `json:"amount"`
	// Reason says why a rejected refund was refused.
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Op = simkit.Op

const (
	OpCreate    Op = "create"
	OpGet       Op = "get"
	OpRefund    Op = "refund"
	OpGetRefund Op = "get_refund"
)

// Ops are the operations a fault can target; Rejectable those that can be refused.
var (
	Ops        = []Op{OpCreate, OpGet, OpRefund, OpGetRefund}
	Rejectable = []Op{OpCreate, OpRefund}
)

type (
	FaultKind = simkit.FaultKind
	Fault     = simkit.Fault
)

const (
	Reject       = simkit.Reject
	FailBefore   = simkit.FailBefore
	TimeoutAfter = simkit.TimeoutAfter
	Delay        = simkit.Delay
)

// Problem codes the simulator answers with.
const (
	CodeRejected       = "charge_rejected"
	CodeRefundRejected = "refund_rejected"
	CodeRefundNotFound = "refund_not_found"
	CodeNotFound       = "charge_not_found"
	CodeNotPayable     = "charge_not_payable"
	CodeKeyReused      = simkit.CodeKeyReused
	CodeInvalid        = simkit.CodeInvalid
	CodeFailedFault    = simkit.CodeFailedFault
	CodeEventNotFound  = simkit.CodeEventNotFound
	CodeDeliveryFailed = simkit.CodeDeliveryFailed
)
