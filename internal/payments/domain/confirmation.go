package domain

import (
	"fmt"
	"time"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
)

// ConfirmationAccounts: Receivables and FXPayer hold the payer's currency, the others
// the merchant's.
type ConfirmationAccounts struct {
	ProviderReceivables ledger.AccountID
	FXPayer             ledger.AccountID
	FXMerchant          ledger.AccountID
	MerchantPayable     ledger.AccountID
	FeeRevenue          ledger.AccountID
	SpreadRevenue       ledger.AccountID
	Rounding            ledger.AccountID
}

// CheckOnTime goes by the provider's time, not when the news arrived, so a payer who
// paid on time is not punished for a slow event. Late money is decision 8.
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

// ConfirmationReference: a payment that got past its status check still cannot post
// twice (invariant 7).
func ConfirmationReference(id PaymentID) string { return "payment:" + id.String() + ":confirm" }

func Confirmation(id PaymentID, q *fx.Quote, a *ConfirmationAccounts) (ledger.Transaction, error) {
	return postLegs(ConfirmationReference(id),
		leg{a.ProviderReceivables, q.PayerAmount(), false},
		leg{a.FXPayer, q.PayerAmount(), true},
		leg{a.FXMerchant, q.Converted(), true},
		leg{a.MerchantPayable, q.MerchantNet(), false},
		leg{a.FeeRevenue, q.Fee(), false},
		leg{a.SpreadRevenue, q.Spread(), false},
		leg{a.Rounding, q.Rounding(), false})
}
