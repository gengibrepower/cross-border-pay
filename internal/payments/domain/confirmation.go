package domain

import (
	"fmt"
	"time"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

// ConfirmationAccounts are where a confirmation posts (section 3.3). Receivables and
// FXPayer hold the payer's currency; the others hold the merchant's.
type ConfirmationAccounts struct {
	ProviderReceivables ledger.AccountID
	FXPayer             ledger.AccountID
	FXMerchant          ledger.AccountID
	MerchantPayable     ledger.AccountID
	FeeRevenue          ledger.AccountID
	SpreadRevenue       ledger.AccountID
	Rounding            ledger.AccountID
}

// CheckOnTime refuses a payment the provider says was made once the quote had expired,
// or without saying when: that is late money (decision 8), not a confirmation. The
// provider's time decides, not when the news reached the gateway, so a payer who paid
// on time is not punished for a slow event.
func CheckOnTime(q *fx.Quote, paidAt time.Time) error {
	if paidAt.IsZero() {
		return fmt.Errorf("%w: the provider did not say when it was paid", ErrLatePayment)
	}
	if q.Expired(paidAt) {
		return fmt.Errorf("%w: paid at %s, quote expired at %s", ErrLatePayment,
			paidAt.UTC().Format(time.RFC3339Nano), q.ExpiresAt().UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// ConfirmationReference is unique in the ledger, so a payment that somehow got past its
// status check still cannot be posted twice (invariant 7).
func ConfirmationReference(id PaymentID) string { return "payment:" + id.String() + ":confirm" }

// Confirmation is the ledger transaction of a paid payment: what the payer paid enters
// at the provider and is converted at the base rate into what the merchant is owed,
// the fee, the spread and the rounding. Zero legs are left out; the ledger refuses
// them.
func Confirmation(id PaymentID, q *fx.Quote, a *ConfirmationAccounts) (ledger.Transaction, error) {
	paid, err := q.PayerAmount().Neg()
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("payments: confirm %s: %w", id, err)
	}
	converted, err := q.Converted().Neg()
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("payments: confirm %s: %w", id, err)
	}
	var entries []ledger.Entry
	for _, leg := range []struct {
		account ledger.AccountID
		amount  money.Money
	}{
		{a.ProviderReceivables, q.PayerAmount()},
		{a.FXPayer, paid},
		{a.FXMerchant, converted},
		{a.MerchantPayable, q.MerchantNet()},
		{a.FeeRevenue, q.Fee()},
		{a.SpreadRevenue, q.Spread()},
		{a.Rounding, q.Rounding()},
	} {
		if !leg.amount.IsZero() {
			entries = append(entries, ledger.NewEntry(leg.account, leg.amount))
		}
	}
	t, err := ledger.NewTransaction(ConfirmationReference(id), entries...)
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("payments: confirm %s: %w", id, err)
	}
	return t, nil
}
