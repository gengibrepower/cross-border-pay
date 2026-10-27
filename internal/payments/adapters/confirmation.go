package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	fxadapters "github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	ledgeradapters "github.com/gengibrepower/cross-border-pay/internal/ledger/adapters"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// confirm moves the locked payment to paid, posts its ledger transaction and appends
// payment.paid to the outbox, all in the attempt's transaction (section 4.1, step 6). A
// payment already paid is a no-op.
func confirm(ctx context.Context, tx pgx.Tx, outbox Outbox, pay *paymentsdb.LockPaymentOfAttemptRow, now time.Time) error {
	status, err := domain.ParseStatus(pay.Status)
	if err != nil {
		return fmt.Errorf("stored payment: %w", err)
	}
	to, changed, err := domain.Transition(status, domain.Confirm)
	if err != nil {
		return fmt.Errorf("payment: %w", err)
	}
	if !changed {
		return nil
	}
	q := paymentsdb.New(tx)
	version, err := q.UpdatePaymentStatus(ctx, paymentsdb.UpdatePaymentStatusParams{
		ID:        pay.PaymentID,
		Status:    string(to),
		UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("update payment: %w", err)
	}

	quoteID := fx.QuoteID(pay.QuoteID.Bytes)
	quotes, err := fxadapters.LoadQuotes(ctx, tx, []fx.QuoteID{quoteID})
	if err != nil {
		return fmt.Errorf("quote: %w", err)
	}
	quote, ok := quotes[quoteID]
	if !ok {
		return fmt.Errorf("quote %x not found", quoteID)
	}
	accounts, err := confirmationAccounts(ctx, tx, pay.MerchantID.Bytes, &quote)
	if err != nil {
		return err
	}
	t, err := domain.Confirmation(pay.PaymentID.Bytes, &quote, &accounts)
	if err != nil {
		return fmt.Errorf("transaction: %w", err)
	}
	if _, err := (ledgeradapters.Ledger{}).Post(ctx, tx, t); err != nil {
		return fmt.Errorf("post: %w", err)
	}
	return notify(ctx, tx, outbox, pay, to, int(version), now)
}

func notify(ctx context.Context, tx pgx.Tx, outbox Outbox, pay *paymentsdb.LockPaymentOfAttemptRow, status domain.Status, version int, now time.Time) error {
	currency, err := money.ParseCurrency(pay.Currency)
	if err != nil {
		return fmt.Errorf("stored payment: %w", err)
	}
	id, err := paymentsdb.New(tx).NewEventID(ctx)
	if err != nil {
		return fmt.Errorf("event ID: %w", err)
	}
	e, err := domain.NewPaymentEvent(id.Bytes, domain.PaymentPaid, &domain.PaymentSnapshot{
		ID:       pay.PaymentID.Bytes,
		Merchant: pay.MerchantID.Bytes,
		Version:  version,
		Status:   status,
		Amount:   money.New(pay.Amount, currency),
	}, now)
	if err != nil {
		return fmt.Errorf("event: %w", err)
	}
	if err := outbox.Append(ctx, tx, &e); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	return nil
}

// confirmationAccounts finds the chart of accounts' rows (migration 00009) for the
// quote's currencies, and the merchant's payable, created on its first confirmation.
func confirmationAccounts(ctx context.Context, tx pgx.Tx, merchant domain.MerchantID, q *fx.Quote) (domain.ConfirmationAccounts, error) {
	payer, base := q.PayerAmount().Currency(), q.Amount().Currency()
	receivables, fxPayer, fxBase := "provider_receivables:"+payer.Code(), "fx:"+payer.Code(), "fx:"+base.Code()
	fee, spread, rounding := "fee_revenue:"+base.Code(), "spread_revenue:"+base.Code(), "rounding:"+base.Code()

	l := ledgeradapters.Ledger{}
	system, err := l.AccountsByCode(ctx, tx, receivables, fxPayer, fxBase, fee, spread, rounding)
	if err != nil {
		return domain.ConfirmationAccounts{}, fmt.Errorf("chart of accounts: %w", err)
	}
	payable, err := l.EnsureAccount(ctx, tx, fmt.Sprintf("merchant_payable:%x:%s", merchant, base.Code()), base, ledger.MayGoNegative)
	if err != nil {
		return domain.ConfirmationAccounts{}, fmt.Errorf("merchant payable: %w", err)
	}
	return domain.ConfirmationAccounts{
		ProviderReceivables: system[receivables].ID(),
		FXPayer:             system[fxPayer].ID(),
		FXMerchant:          system[fxBase].ID(),
		MerchantPayable:     payable.ID(),
		FeeRevenue:          system[fee].ID(),
		SpreadRevenue:       system[spread].ID(),
		Rounding:            system[rounding].ID(),
	}, nil
}
