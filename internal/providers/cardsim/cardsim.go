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

type (
	Op        = simkit.Op
	FaultKind = simkit.FaultKind
	Fault     = simkit.Fault
)

const (
	OpAuthorize Op = "authorize"
	OpGet       Op = "get"
	OpCapture   Op = "capture"
	OpRefund    Op = "refund"
	OpGetRefund Op = "get_refund"
)

// Ops are the operations a fault can target. A decline is the issuer's answer to an
// authorization, and a refusal the acquirer's to a refund; a capture of a live
// authorization is never refused, only cut short.
var (
	Ops        = []Op{OpAuthorize, OpGet, OpCapture, OpRefund, OpGetRefund}
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
	CodeRefundRejected = "refund_rejected"
	CodeRefundNotFound = "refund_not_found"
	CodeKeyReused      = simkit.CodeKeyReused
	CodeInvalid        = simkit.CodeInvalid
)
