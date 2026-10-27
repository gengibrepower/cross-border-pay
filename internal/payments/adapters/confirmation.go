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

// confirm runs when the attempt has just succeeded. It moves the locked payment to paid,
// posts its ledger transaction and appends payment.paid to the outbox, all in the
// attempt's transaction (section 4.1, step 6). Money that cannot confirm the payment,
// late, for another amount or for a payment already paid, is posted as a divergence.
func confirm(ctx context.Context, tx pgx.Tx, sinks *Sinks, pay *paymentsdb.LockPaymentOfAttemptRow, attempt *domain.PaymentAttempt, u *domain.AttemptUpdate, now time.Time) error {
	status, err := domain.ParseStatus(pay.Status)
	if err != nil {
		return fmt.Errorf("stored payment: %w", err)
	}
	quote, err := loadQuote(ctx, tx, pay.QuoteID.Bytes)
	if err != nil {
		return err
	}
	to, err := domain.ConfirmPayment(status, &quote, u.Amount, u.PaidAt)
	if kind, ok := domain.DivergenceOf(err); ok {
		return diverge(ctx, tx, sinks.Reviews, pay, attempt, kind, u, err, now)
	}
	if err != nil {
		return fmt.Errorf("payment: %w", err)
	}
	version, err := setStatus(ctx, tx, pay.PaymentID, to, now)
	if err != nil {
		return err
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
	return notify(ctx, tx, sinks.Outbox, domain.PaymentPaid, &storedPayment{
		id: pay.PaymentID, merchant: pay.MerchantID, amount: pay.Amount, currency: pay.Currency,
	}, to, version, now)
}

func loadQuote(ctx context.Context, tx pgx.Tx, id fx.QuoteID) (fx.Quote, error) {
	quotes, err := fxadapters.LoadQuotes(ctx, tx, []fx.QuoteID{id})
	if err != nil {
		return fx.Quote{}, fmt.Errorf("quote: %w", err)
	}
	quote, ok := quotes[id]
	if !ok {
		return fx.Quote{}, fmt.Errorf("quote %x not found", id)
	}
	return quote, nil
}

// setStatus also bumps the version the payment's events carry, and takes the payment
// out of the expiry queue: only a pending payment can expire.
func setStatus(ctx context.Context, tx pgx.Tx, id pgtype.UUID, to domain.Status, now time.Time) (int, error) {
	version, err := paymentsdb.New(tx).UpdatePaymentStatus(ctx, paymentsdb.UpdatePaymentStatusParams{
		ID:        id,
		Status:    string(to),
		UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return 0, fmt.Errorf("update payment: %w", err)
	}
	return int(version), nil
}

// storedPayment is what an event needs from a payment's row.
type storedPayment struct {
	id, merchant pgtype.UUID
	amount       int64
	currency     string
}

func notify(ctx context.Context, tx pgx.Tx, outbox Outbox, t domain.EventType, p *storedPayment, status domain.Status, version int, now time.Time) error {
	currency, err := money.ParseCurrency(p.currency)
	if err != nil {
		return fmt.Errorf("stored payment: %w", err)
	}
	id, err := paymentsdb.New(tx).NewEventID(ctx)
	if err != nil {
		return fmt.Errorf("event ID: %w", err)
	}
	e, err := domain.NewPaymentEvent(id.Bytes, t, &domain.PaymentSnapshot{
		ID:       p.id.Bytes,
		Merchant: p.merchant.Bytes,
		Version:  version,
		Status:   status,
		Amount:   money.New(p.amount, currency),
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
