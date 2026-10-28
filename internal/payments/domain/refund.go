package domain

import (
	"errors"
	"fmt"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/publicid"
)

var (
	ErrInvalidRefundID = errors.New("payments: invalid refund ID")
	// ErrRefundExceedsPayment is invariant 8: refunds never return more than was paid.
	ErrRefundExceedsPayment = errors.New("payments: refunds would exceed the amount paid")
	// ErrRefundTooSmall is a refund whose share of what the payer paid rounds to nothing.
	ErrRefundTooSmall = errors.New("payments: refund too small to return anything to the payer")
)

type RefundID [16]byte

func (id RefundID) String() string { return publicid.Encode("ref_", id) }

func ParseRefundID(s string) (RefundID, error) {
	id, ok := publicid.Parse("ref_", s)
	if !ok {
		return RefundID{}, fmt.Errorf("%w: %q", ErrInvalidRefundID, s)
	}
	return id, nil
}

// RefundAmounts is what one refund moves, fixed when it is created and stored with it.
type RefundAmounts struct {
	// Amount is what the merchant refunds, in the merchant's currency; it leaves the
	// merchant's payable whole, because the fee is not returned (F7, decision A).
	Amount money.Money
	// PayerAmount is what goes back to the payer, in the payer's currency.
	PayerAmount money.Money
	// Spread is the share of the spread revenue given back with it. It can be negative
	// by a cent, when the shares of earlier refunds rounded up.
	Spread money.Money
}

// PriceRefund prices a refund of amount by the proportion of the original payment
// (decision 13). before is the sum of the payment's refunds that still count, every
// one but the rejected.
//
// Each share is cumulative: what all refunds up to this one should have returned,
// minus what the earlier ones did. Rounding each refund on its own would drift: two
// halves of R$ 101.51 are R$ 50.76 each, R$ 101.52 in all, more than the payer paid.
// Cumulative shares always add up to the exact total.
func PriceRefund(q *fx.Quote, before *RefundAmounts, amount money.Money) (RefundAmounts, error) {
	if amount.Currency() != q.Amount().Currency() {
		return RefundAmounts{}, fmt.Errorf("payments: refund %s: %w: the payment is in %s", amount, money.ErrCurrencyMismatch, q.Amount().Currency())
	}
	if amount.Amount() <= 0 {
		return RefundAmounts{}, fmt.Errorf("%w: got %s", ErrInvalidAmount, amount)
	}
	after, err := before.Amount.Add(amount)
	if err != nil {
		return RefundAmounts{}, fmt.Errorf("payments: refund %s: %w", amount, err)
	}
	if after.Amount() > q.Amount().Amount() {
		return RefundAmounts{}, fmt.Errorf("%w: %s already refunded, %s more, %s paid", ErrRefundExceedsPayment, before.Amount, amount, q.Amount())
	}
	payer, err := cumulativeShare(q.PayerAmount(), before.PayerAmount, after.Amount(), q.Amount().Amount())
	if err != nil {
		return RefundAmounts{}, err
	}
	if payer.Amount() <= 0 {
		return RefundAmounts{}, fmt.Errorf("%w: %s of %s", ErrRefundTooSmall, amount, q.Amount())
	}
	spread, err := cumulativeShare(q.Spread(), before.Spread, after.Amount(), q.Amount().Amount())
	if err != nil {
		return RefundAmounts{}, err
	}
	return RefundAmounts{Amount: amount, PayerAmount: payer, Spread: spread}, nil
}

func cumulativeShare(total, returned money.Money, part, whole int64) (money.Money, error) {
	due, err := fx.Share(total, part, whole)
	if err != nil {
		return money.Money{}, fmt.Errorf("payments: refund share: %w", err)
	}
	share, err := due.Sub(returned)
	if err != nil {
		return money.Money{}, fmt.Errorf("payments: refund share: %w", err)
	}
	return share, nil
}

// Add sums two refunds, for the running total PriceRefund takes.
func (r *RefundAmounts) Add(other *RefundAmounts) (RefundAmounts, error) {
	amount, err := r.Amount.Add(other.Amount)
	if err != nil {
		return RefundAmounts{}, fmt.Errorf("payments: refund total: %w", err)
	}
	payer, err := r.PayerAmount.Add(other.PayerAmount)
	if err != nil {
		return RefundAmounts{}, fmt.Errorf("payments: refund total: %w", err)
	}
	spread, err := r.Spread.Add(other.Spread)
	if err != nil {
		return RefundAmounts{}, fmt.Errorf("payments: refund total: %w", err)
	}
	return RefundAmounts{Amount: amount, PayerAmount: payer, Spread: spread}, nil
}

// NoRefunds is the running total of a payment nothing was refunded of yet.
func NoRefunds(q *fx.Quote) RefundAmounts {
	return RefundAmounts{
		Amount:      money.Zero(q.Amount().Currency()),
		PayerAmount: money.Zero(q.PayerAmount().Currency()),
		Spread:      money.Zero(q.Spread().Currency()),
	}
}

// RefundAccounts are where a refund posts (section 3.3).
type RefundAccounts struct {
	ProviderReceivables ledger.AccountID
	FXPayer             ledger.AccountID
	FXMerchant          ledger.AccountID
	MerchantPayable     ledger.AccountID
	// RefundsInFlight holds what refunds took from merchants until the provider
	// answers, so the merchant's balance never counts money on its way back.
	RefundsInFlight ledger.AccountID
	SpreadRevenue   ledger.AccountID
	FXResult        ledger.AccountID
}

type RefundStep string

const (
	RefundHold     RefundStep = "hold"
	RefundComplete RefundStep = "complete"
	RefundRelease  RefundStep = "release"
)

// RefundReference is unique in the ledger, so each step of a refund posts once.
func RefundReference(id RefundID, step RefundStep) string {
	return "refund:" + id.String() + ":" + string(step)
}

// RefundHoldPosting takes the refund out of the merchant's payable when it is created,
// before the provider is called: the payable can go negative (decision 27), the money
// in flight cannot.
func RefundHoldPosting(id RefundID, r *RefundAmounts, a *RefundAccounts) (ledger.Transaction, error) {
	return postLegs(RefundReference(id, RefundHold),
		leg{a.MerchantPayable, r.Amount, true},
		leg{a.RefundsInFlight, r.Amount, false})
}

// RefundReleasePosting gives the hold back to the merchant when the provider refuses.
func RefundReleasePosting(id RefundID, r *RefundAmounts, a *RefundAccounts) (ledger.Transaction, error) {
	return postLegs(RefundReference(id, RefundRelease),
		leg{a.RefundsInFlight, r.Amount, true},
		leg{a.MerchantPayable, r.Amount, false})
}

// RefundCompletionPosting is the money gone back to the payer. The payer's currency is
// bought back at rate, today's base rate; the merchant paid the refunded amount and the
// spread returns its share, so the difference is the exchange result the gateway
// absorbs (decision 13): a gain when the payer's currency got cheaper.
func RefundCompletionPosting(id RefundID, r *RefundAmounts, rate fx.Rate, a *RefundAccounts) (ledger.Transaction, error) {
	cost, err := fx.Convert(r.PayerAmount, rate)
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("payments: complete refund %s: %w", id, err)
	}
	held, err := r.Amount.Add(r.Spread)
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("payments: complete refund %s: %w", id, err)
	}
	result, err := held.Sub(cost)
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("payments: complete refund %s: %w", id, err)
	}
	return postLegs(RefundReference(id, RefundComplete),
		leg{a.ProviderReceivables, r.PayerAmount, true},
		leg{a.FXPayer, r.PayerAmount, false},
		leg{a.FXMerchant, cost, false},
		leg{a.RefundsInFlight, r.Amount, true},
		leg{a.SpreadRevenue, r.Spread, true},
		leg{a.FXResult, result, false})
}

type leg struct {
	account ledger.AccountID
	amount  money.Money
	out     bool
}

// postLegs builds the transaction, leaving zero legs out: the ledger refuses them.
func postLegs(reference string, legs ...leg) (ledger.Transaction, error) {
	var entries []ledger.Entry
	for _, l := range legs {
		if l.amount.IsZero() {
			continue
		}
		amount := l.amount
		if l.out {
			neg, err := amount.Neg()
			if err != nil {
				return ledger.Transaction{}, fmt.Errorf("payments: %s: %w", reference, err)
			}
			amount = neg
		}
		entries = append(entries, ledger.NewEntry(l.account, amount))
	}
	t, err := ledger.NewTransaction(reference, entries...)
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("payments: %s: %w", reference, err)
	}
	return t, nil
}
