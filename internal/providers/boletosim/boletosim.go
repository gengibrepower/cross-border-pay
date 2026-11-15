// Package boletosim is the client of the boleto simulator in cmd/boletosim. It speaks
// only the simulator's own types; payments translates them.
package boletosim

import (
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

// Payer is decision 54. The simulator never sends the CPF back.
type Payer struct {
	Name     string `json:"name"`
	Document string `json:"document"`
	Email    string `json:"email,omitempty"`
}

// CreateBoleto is received until the end of the first banking day on or after
// DueDate (decision 53).
type CreateBoleto struct {
	Amount  money.Money   `json:"amount"`
	DueDate calendar.Date `json:"due_date"`
	Payer   Payer         `json:"payer"`
}

type Status string

const (
	Registered Status = "registered"
	Paid       Status = "paid"
	Rejected   Status = "rejected"
	// Expired: nothing was paid and nothing can be.
	Expired Status = "expired"
)

type Boleto struct {
	ID             string        `json:"id"`
	IdempotencyKey string        `json:"idempotency_key"`
	Status         Status        `json:"status"`
	Amount         money.Money   `json:"amount"`
	DueDate        calendar.Date `json:"due_date"`
	// NossoNumero, Barcode and DigitableLine are empty on a rejected boleto.
	NossoNumero   string     `json:"nosso_numero,omitempty"`
	Barcode       string     `json:"barcode,omitempty"`
	DigitableLine string     `json:"digitable_line,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	PaidAt        *time.Time `json:"paid_at,omitempty"`
}

// CreateRefund: a boleto has no way back of its own, so the bank pays the payer by
// transfer.
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
	BoletoID       string       `json:"boleto_id"`
	Status         RefundStatus `json:"status"`
	Amount         money.Money  `json:"amount"`
	Reason         string       `json:"reason,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
}

type (
	Op        = simkit.Op
	FaultKind = simkit.FaultKind
	Fault     = simkit.Fault
	Statement = simkit.Statement
	Movement  = simkit.Movement
)

const (
	OpCreate    Op = "create"
	OpGet       Op = "get"
	OpRefund    Op = "refund"
	OpGetRefund Op = "get_refund"
	OpStatement    = simkit.OpStatement
)

var (
	Ops        = []Op{OpCreate, OpGet, OpRefund, OpGetRefund, OpStatement}
	Rejectable = []Op{OpCreate, OpRefund}
)

const (
	Reject       = simkit.Reject
	FailBefore   = simkit.FailBefore
	TimeoutAfter = simkit.TimeoutAfter
	Delay        = simkit.Delay
)

const (
	CodeRejected       = "boleto_rejected"
	CodeNotFound       = "boleto_not_found"
	CodeNotPayable     = "boleto_not_payable"
	CodeOverdue        = "boleto_overdue"
	CodeRefundRejected = "refund_rejected"
	CodeRefundNotFound = "refund_not_found"
	CodeKeyReused      = simkit.CodeKeyReused
	CodeInvalid        = simkit.CodeInvalid
	CodeEventNotFound  = simkit.CodeEventNotFound
	CodeDeliveryFailed = simkit.CodeDeliveryFailed
)
