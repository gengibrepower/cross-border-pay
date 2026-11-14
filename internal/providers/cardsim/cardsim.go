// Package cardsim is the client of the card simulator in cmd/cardsim: an acquirer that
// authorizes, captures and refunds fictitious card tokens, and fails on command. It
// speaks only the simulator's own types; payments translates them.
package cardsim

import (
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

// Test tokens. No card number ever reaches the gateway or the simulator (section 7,
// PCI DSS): a token stands for a card, and its name says how the issuer answers. Any
// other token is declined as invalid.
const (
	TokenApproved          = "tok_approved"
	TokenDeclined          = "tok_declined"
	TokenInsufficientFunds = "tok_insufficient_funds"
)

// AuthorizationValidity is how long an authorization can be captured (decision 52).
const AuthorizationValidity = 7 * 24 * time.Hour

// Authorize asks the issuer to hold Amount on the card. Capture true captures it
// whole at once (decision 18's default); false leaves it to Capture.
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
	// Expired is an authorization never captured within AuthorizationValidity: the
	// hold is gone and nothing was charged.
	Expired Status = "expired"
	// Voided is an authorization released before capture: the hold is gone at once.
	Voided Status = "voided"
)

type Authorization struct {
	ID             string      `json:"id"`
	IdempotencyKey string      `json:"idempotency_key"`
	Status         Status      `json:"status"`
	Amount         money.Money `json:"amount"`
	// Captured is what was charged, at most Amount; zero until captured.
	Captured money.Money `json:"captured"`
	// DeclineReason says why a declined authorization was refused.
	DeclineReason string     `json:"decline_reason,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	CapturedAt    *time.Time `json:"captured_at,omitempty"`
}

// Capture charges part or all of an authorization, once: capturing again returns the
// same authorization, and another amount is refused.
type Capture struct {
	Amount money.Money `json:"amount"`
}

// CreateRefund returns part or all of a captured authorization to the card.
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

// ChargebackStatus is where a chargeback is in the card network's dispute: the
// cardholder's bank took the money back, and the merchant may answer.
type ChargebackStatus string

const (
	// NeedsResponse waits for the merchant to accept or contest.
	NeedsResponse ChargebackStatus = "needs_response"
	// UnderReview is contested, waiting for the issuer's decision.
	UnderReview ChargebackStatus = "under_review"
	// Won gives the money back to the merchant; Lost keeps it with the cardholder,
	// accepted or decided against.
	Won  ChargebackStatus = "won"
	Lost ChargebackStatus = "lost"
)

// Chargeback is the cardholder's bank taking back Amount of a captured authorization,
// for Reason. AuthorizationKey is the authorization's idempotency key, so whoever
// created it finds its own record.
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

// OpenChargeback is the control request that makes a cardholder dispute a capture.
type OpenChargeback struct {
	Amount money.Money `json:"amount"`
	Reason string      `json:"reason"`
}

// Contest answers a chargeback with the merchant's evidence.
type Contest struct {
	Evidence string `json:"evidence"`
}

// ResolveChargeback is the control request standing in for the issuer's decision:
// Won or Lost.
type ResolveChargeback struct {
	Outcome ChargebackStatus `json:"outcome"`
}

// MaxReason and MaxEvidence bound the texts a chargeback carries.
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
	OpAuthorize Op = "authorize"
	OpGet       Op = "get"
	OpCapture   Op = "capture"
	OpVoid      Op = "void"
	OpRefund    Op = "refund"
	OpGetRefund Op = "get_refund"
	// OpRespond is accepting or contesting a chargeback.
	OpRespond       Op = "respond"
	OpGetChargeback Op = "get_chargeback"
)

// Ops are the operations a fault can target. A decline is the issuer's answer to an
// authorization, and a refusal the acquirer's to a refund; a capture of a live
// authorization is never refused, only cut short.
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

// Problem codes the simulator answers with.
const (
	CodeNotFound       = "authorization_not_found"
	CodeNotCapturable  = "authorization_not_capturable"
	CodeNotVoidable    = "authorization_not_voidable"
	CodeRefundRejected = "refund_rejected"
	CodeRefundNotFound = "refund_not_found"
	// CodeNotDisputable is a chargeback of an authorization not captured, or of more
	// than is left of its capture.
	CodeNotDisputable      = "authorization_not_disputable"
	CodeChargebackNotFound = "chargeback_not_found"
	// CodeChargebackClosed is an answer the chargeback can no longer take.
	CodeChargebackClosed = "chargeback_closed"
	CodeEventNotFound    = simkit.CodeEventNotFound
	CodeDeliveryFailed   = simkit.CodeDeliveryFailed
	CodeKeyReused        = simkit.CodeKeyReused
	CodeInvalid          = simkit.CodeInvalid
)
