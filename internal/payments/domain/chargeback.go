package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/publicid"
)

var (
	ErrInvalidChargebackID = errors.New("payments: invalid chargeback ID")
	// The acquirer holds the same bound, so this is a provider event that cannot apply.
	ErrChargebackExceedsPayment = errors.New("payments: chargebacks exceed what was paid")
	// ErrChargebackClosed is also an answer contradicting what the provider decided.
	ErrChargebackClosed  = errors.New("payments: chargeback is closed")
	ErrUnknownChargeback = errors.New("payments: unknown chargeback status")
	ErrInvalidReason     = errors.New("payments: chargeback reason must have 1 to 100 characters")
	ErrInvalidEvidence   = errors.New("payments: evidence must have 1 to 2000 characters")
)

const (
	maxReason   = 100
	maxEvidence = 2000
)

type ChargebackID [16]byte

func (id ChargebackID) String() string { return publicid.Encode("cbk_", id) }

func ParseChargebackID(s string) (ChargebackID, error) {
	id, ok := publicid.Parse("cbk_", s)
	if !ok {
		return ChargebackID{}, fmt.Errorf("%w: %q", ErrInvalidChargebackID, s)
	}
	return id, nil
}

// ChargebackStatus is section 4.7: the merchant accepts and loses the money, or
// contests and the issuer decides.
type ChargebackStatus string

const (
	NeedsResponse ChargebackStatus = "needs_response"
	UnderReview   ChargebackStatus = "under_review"
	Won           ChargebackStatus = "won"
	Lost          ChargebackStatus = "lost"
)

func ParseChargebackStatus(s string) (ChargebackStatus, error) {
	switch st := ChargebackStatus(s); st {
	case NeedsResponse, UnderReview, Won, Lost:
		return st, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownChargeback, s)
	}
}

func (s ChargebackStatus) Closed() bool { return s == Won || s == Lost }

func (s ChargebackStatus) stage() int {
	switch s {
	case NeedsResponse:
		return 0
	case UnderReview:
		return 1
	case Won, Lost:
		return 2
	default:
		return -1
	}
}

// MoveChargeback takes the provider's word as long as the dispute moves forward: a
// report behind what is stored is an older answer and changes nothing. Only
// needs_response can be lost without a review, when nobody answered in time.
func MoveChargeback(from, to ChargebackStatus) (ChargebackStatus, bool, error) {
	if from.stage() < 0 || to.stage() < 0 {
		return from, false, fmt.Errorf("%w: %q to %q", ErrUnknownChargeback, from, to)
	}
	switch {
	case from == to:
		return from, false, nil
	case from.Closed():
		return from, false, fmt.Errorf("%w: it is %s, the provider says %s", ErrChargebackClosed, from, to)
	case to.stage() < from.stage():
		return from, false, nil
	case from == NeedsResponse && to == Won:
		return from, false, fmt.Errorf("%w: a chargeback nobody contested cannot be won", ErrChargebackClosed)
	default:
		return to, true, nil
	}
}

func NewReason(s string) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > maxReason {
		return "", fmt.Errorf("%w: got %d", ErrInvalidReason, n)
	}
	return s, nil
}

func NewEvidence(s string) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > maxEvidence {
		return "", fmt.Errorf("%w: got %d", ErrInvalidEvidence, n)
	}
	return s, nil
}

// ChargebackAmounts is fixed when the chargeback opens. Amount is the merchant's
// share of the payment, with the fee kept as for a refund.
type ChargebackAmounts struct {
	PayerAmount money.Money
	Amount      money.Money
	Spread      money.Money
}

func NoChargebacks(q *fx.Quote) ChargebackAmounts {
	return ChargebackAmounts{
		PayerAmount: money.Zero(q.PayerAmount().Currency()),
		Amount:      money.Zero(q.Amount().Currency()),
		Spread:      money.Zero(q.Spread().Currency()),
	}
}

func (c *ChargebackAmounts) Add(other *ChargebackAmounts) (ChargebackAmounts, error) {
	payer, err := c.PayerAmount.Add(other.PayerAmount)
	if err != nil {
		return ChargebackAmounts{}, fmt.Errorf("payments: chargeback total: %w", err)
	}
	amount, err := c.Amount.Add(other.Amount)
	if err != nil {
		return ChargebackAmounts{}, fmt.Errorf("payments: chargeback total: %w", err)
	}
	spread, err := c.Spread.Add(other.Spread)
	if err != nil {
		return ChargebackAmounts{}, fmt.Errorf("payments: chargeback total: %w", err)
	}
	return ChargebackAmounts{PayerAmount: payer, Amount: amount, Spread: spread}, nil
}

// PriceChargeback is PriceRefund from the payer's side (decision 13): the bank says how
// much BRL left, and the merchant loses that share of the payment. before sums only
// earlier chargebacks: a refunded cardholder can still charge back, and the money then
// leaves twice (decision 7).
func PriceChargeback(q *fx.Quote, before *ChargebackAmounts, payer money.Money) (ChargebackAmounts, error) {
	if payer.Currency() != q.PayerAmount().Currency() {
		return ChargebackAmounts{}, fmt.Errorf("payments: chargeback %s: %w: the payer paid in %s", payer, money.ErrCurrencyMismatch, q.PayerAmount().Currency())
	}
	if payer.Amount() <= 0 {
		return ChargebackAmounts{}, fmt.Errorf("%w: got %s", ErrInvalidAmount, payer)
	}
	after, err := before.PayerAmount.Add(payer)
	if err != nil {
		return ChargebackAmounts{}, fmt.Errorf("payments: chargeback %s: %w", payer, err)
	}
	if after.Amount() > q.PayerAmount().Amount() {
		return ChargebackAmounts{}, fmt.Errorf("%w: %s already taken back, %s more, %s paid", ErrChargebackExceedsPayment, before.PayerAmount, payer, q.PayerAmount())
	}
	amount, err := cumulativeShare(q.Amount(), before.Amount, after.Amount(), q.PayerAmount().Amount())
	if err != nil {
		return ChargebackAmounts{}, err
	}
	spread, err := cumulativeShare(q.Spread(), before.Spread, after.Amount(), q.PayerAmount().Amount())
	if err != nil {
		return ChargebackAmounts{}, err
	}
	return ChargebackAmounts{PayerAmount: payer, Amount: amount, Spread: spread}, nil
}

type ChargebackAccounts struct {
	ProviderReceivables ledger.AccountID
	FXPayer             ledger.AccountID
	FXMerchant          ledger.AccountID
	MerchantPayable     ledger.AccountID
	SpreadRevenue       ledger.AccountID
	FXResult            ledger.AccountID
}

type ChargebackStep string

const (
	ChargebackDebit    ChargebackStep = "debit"
	ChargebackReversal ChargebackStep = "reversal"
)

// ChargebackReference is unique in the ledger, so each step of a chargeback posts once.
func ChargebackReference(id ChargebackID, step ChargebackStep) string {
	return "chargeback:" + id.String() + ":" + string(step)
}

// ChargebackDebitPosting posts when the chargeback opens, since the money is gone
// whatever the dispute decides. It is a refund completed at once at today's base rate,
// out of a payable that can go negative (decision 27), with the exchange result the
// gateway's (decision 13).
func ChargebackDebitPosting(id ChargebackID, c *ChargebackAmounts, rate fx.Rate, a *ChargebackAccounts) (ledger.Transaction, error) {
	cost, result, err := chargebackCost(id, c, rate)
	if err != nil {
		return ledger.Transaction{}, err
	}
	return postLegs(ChargebackReference(id, ChargebackDebit),
		leg{a.ProviderReceivables, c.PayerAmount, true},
		leg{a.FXPayer, c.PayerAmount, false},
		leg{a.FXMerchant, cost, false},
		leg{a.MerchantPayable, c.Amount, true},
		leg{a.SpreadRevenue, c.Spread, true},
		leg{a.FXResult, result, false})
}

// ChargebackReversalPosting is a won dispute: the merchant and the spread get back
// exactly what they lost, and the exchange result takes today's difference again.
func ChargebackReversalPosting(id ChargebackID, c *ChargebackAmounts, rate fx.Rate, a *ChargebackAccounts) (ledger.Transaction, error) {
	cost, result, err := chargebackCost(id, c, rate)
	if err != nil {
		return ledger.Transaction{}, err
	}
	return postLegs(ChargebackReference(id, ChargebackReversal),
		leg{a.ProviderReceivables, c.PayerAmount, false},
		leg{a.FXPayer, c.PayerAmount, true},
		leg{a.FXMerchant, cost, true},
		leg{a.MerchantPayable, c.Amount, false},
		leg{a.SpreadRevenue, c.Spread, false},
		leg{a.FXResult, result, true})
}

func chargebackCost(id ChargebackID, c *ChargebackAmounts, rate fx.Rate) (cost, result money.Money, err error) {
	cost, err = fx.Convert(c.PayerAmount, rate)
	if err != nil {
		return money.Money{}, money.Money{}, fmt.Errorf("payments: chargeback %s: %w", id, err)
	}
	shares, err := c.Amount.Add(c.Spread)
	if err != nil {
		return money.Money{}, money.Money{}, fmt.Errorf("payments: chargeback %s: %w", id, err)
	}
	result, err = shares.Sub(cost)
	if err != nil {
		return money.Money{}, money.Money{}, fmt.Errorf("payments: chargeback %s: %w", id, err)
	}
	return cost, result, nil
}

type ChargebackCase struct {
	ID         ChargebackID
	Payment    PaymentID
	Merchant   MerchantID
	Provider   ProviderName
	ExternalID string
	Reason     string
	Status     ChargebackStatus
	Amounts    ChargebackAmounts
	Evidence   string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ClosedAt   time.Time
}
