// Package cardsim is the client of the card simulator in cmd/cardsim. It speaks only
// the simulator's own types; payments translates them.
package cardsim

import (
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

// Test tokens: no card number ever reaches the gateway or the simulator (section 7,
// PCI DSS). Any other token is declined as invalid.
const (
	TokenApproved          = "tok_approved"
	TokenDeclined          = "tok_declined"
	TokenInsufficientFunds = "tok_insufficient_funds"
)

// AuthorizationValidity is decision 52.
const AuthorizationValidity = 7 * 24 * time.Hour

// Authorize's Capture true captures at once, decision 18's default.
type Authorize struct {
	Amount  money.Money `json:"amount"`
	Token   string      `json:"token"`
	Capture bool        `json:"capture"`
}

type Status string

const (
	Authorized Status = "authorized"
	Captured   Status = "captured"
	Declined   Status = "declined"
	// Expired and Voided charged nothing.
	Expired Status = "expired"
	Voided  Status = "voided"
)

type Authorization struct {
	ID             string      `json:"id"`
	IdempotencyKey string      `json:"idempotency_key"`
	Status         Status      `json:"status"`
	Amount         money.Money `json:"amount"`
	Captured       money.Money `json:"captured"`
	DeclineReason  string      `json:"decline_reason,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	ExpiresAt      time.Time   `json:"expires_at"`
	CapturedAt     *time.Time  `json:"captured_at,omitempty"`
}

// Capture happens once: the same amount again returns the same authorization, and
// another amount is refused.
type Capture struct {
	Amount money.Money `json:"amount"`
}

type CreateRefund struct {
	Amount money.Money `json:"amount"`
}

type RefundStatus string

const (
	RefundCompleted RefundStatus = "completed"
	RefundRejected  RefundStatus = "rejected"
)

type Refund struct {
	ID              string       `json:"id"`
	IdempotencyKey  string       `json:"idempotency_key"`
	AuthorizationID string       `json:"authorization_id"`
	Status          RefundStatus `json:"status"`
	Amount          money.Money  `json:"amount"`
	Reason          string       `json:"reason,omitempty"`
	CreatedAt       time.Time    `json:"created_at"`
}

type ChargebackStatus string

const (
	NeedsResponse ChargebackStatus = "needs_response"
	UnderReview   ChargebackStatus = "under_review"
	Won           ChargebackStatus = "won"
	Lost          ChargebackStatus = "lost"
)

// Chargeback's AuthorizationKey lets whoever created the authorization find its own
// record.
type Chargeback struct {
	ID               string           `json:"id"`
	AuthorizationID  string           `json:"authorization_id"`
	AuthorizationKey string           `json:"authorization_key"`
	Status           ChargebackStatus `json:"status"`
	Amount           money.Money      `json:"amount"`
	Reason           string           `json:"reason"`
	CreatedAt        time.Time        `json:"created_at"`
	ClosedAt         *time.Time       `json:"closed_at,omitempty"`
}

type OpenChargeback struct {
	Amount money.Money `json:"amount"`
	Reason string      `json:"reason"`
}

type Contest struct {
	Evidence string `json:"evidence"`
}

// ResolveChargeback stands in for the issuer's decision.
type ResolveChargeback struct {
	Outcome ChargebackStatus `json:"outcome"`
}

const (
	MaxReason   = 100
	MaxEvidence = 2000
)

type (
	Op        = simkit.Op
	FaultKind = simkit.FaultKind
	Fault     = simkit.Fault
)

const (
	OpAuthorize     Op = "authorize"
	OpGet           Op = "get"
	OpCapture       Op = "capture"
	OpVoid          Op = "void"
	OpRefund        Op = "refund"
	OpGetRefund     Op = "get_refund"
	OpRespond       Op = "respond"
	OpGetChargeback Op = "get_chargeback"
)

// Ops a fault can target. A capture of a live authorization is never refused, only cut
// short.
var (
	Ops        = []Op{OpAuthorize, OpGet, OpCapture, OpVoid, OpRefund, OpGetRefund, OpRespond, OpGetChargeback}
	Rejectable = []Op{OpAuthorize, OpRefund}
)

const (
	Reject       = simkit.Reject
	FailBefore   = simkit.FailBefore
	TimeoutAfter = simkit.TimeoutAfter
	Delay        = simkit.Delay
)

const (
	CodeNotFound           = "authorization_not_found"
	CodeNotCapturable      = "authorization_not_capturable"
	CodeNotVoidable        = "authorization_not_voidable"
	CodeRefundRejected     = "refund_rejected"
	CodeRefundNotFound     = "refund_not_found"
	CodeNotDisputable      = "authorization_not_disputable"
	CodeChargebackNotFound = "chargeback_not_found"
	CodeChargebackClosed   = "chargeback_closed"
	CodeEventNotFound      = simkit.CodeEventNotFound
	CodeDeliveryFailed     = simkit.CodeDeliveryFailed
	CodeKeyReused          = simkit.CodeKeyReused
	CodeInvalid            = simkit.CodeInvalid
)
