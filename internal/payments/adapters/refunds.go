package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	idempotencyadapters "github.com/gengibrepower/cross-border-pay/internal/idempotency/adapters"
	idempotency "github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	ledgeradapters "github.com/gengibrepower/cross-border-pay/internal/ledger/adapters"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

// CreateRefund holds the payment's lock while it sums the refunds that count and
// stores the new one, so concurrent refunds are priced one after another and never
// pass what was paid together (invariant 8).
func (p *Postgres) CreateRefund(ctx context.Context, held *idempotency.Held, merchant domain.MerchantID, payment domain.PaymentID, amount money.Money, provider domain.ProviderName, now time.Time) (domain.Refund, error) {
	var refund domain.Refund
	err := postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		q := paymentsdb.New(tx)
		pay, err := q.LockRefundablePayment(ctx, paymentsdb.LockRefundablePaymentParams{
			MerchantID: pgtype.UUID{Bytes: merchant, Valid: true},
			ID:         pgtype.UUID{Bytes: payment, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrPaymentNotFound
		}
		if err != nil {
			return fmt.Errorf("lock payment: %w", err)
		}
		status, err := domain.ParseStatus(pay.Status)
		if err != nil {
			return fmt.Errorf("stored payment: %w", err)
		}
		if _, _, err := domain.Transition(status, domain.RefundPartial); err != nil {
			return fmt.Errorf("%w: payment is %s", app.ErrPaymentNotRefundable, status)
		}
		quote, err := loadQuote(ctx, tx, pay.QuoteID.Bytes)
		if err != nil {
			return err
		}
		counted, err := q.CountedRefunds(ctx, pay.ID)
		if err != nil {
			return fmt.Errorf("counted refunds: %w", err)
		}
		amounts, err := domain.PriceRefund(&quote, &domain.RefundAmounts{
			Amount:      money.New(counted.Amount, quote.Amount().Currency()),
			PayerAmount: money.New(counted.PayerAmount, quote.PayerAmount().Currency()),
			Spread:      money.New(counted.Spread, quote.Spread().Currency()),
		}, amount)
		if err != nil {
			return fmt.Errorf("price: %w", err)
		}

		row, err := q.InsertRefund(ctx, paymentsdb.InsertRefundParams{
			PaymentID:     pay.ID,
			Provider:      string(provider),
			Amount:        amounts.Amount.Amount(),
			Currency:      amounts.Amount.Currency().Code(),
			PayerAmount:   amounts.PayerAmount.Amount(),
			PayerCurrency: amounts.PayerAmount.Currency().Code(),
			Spread:        amounts.Spread.Amount(),
			CreatedAt:     pgtype.Timestamptz{Time: now, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("insert refund: %w", err)
		}
		refund = domain.NewRefund(row.ID.Bytes, payment, provider, &amounts, row.CreatedAt.Time.UTC())

		accounts, err := refundAccounts(ctx, tx, merchant, &quote)
		if err != nil {
			return err
		}
		hold, err := domain.RefundHoldPosting(refund.ID(), &amounts, &accounts)
		if err != nil {
			return fmt.Errorf("hold: %w", err)
		}
		if _, err := (ledgeradapters.Ledger{}).Post(ctx, tx, hold); err != nil {
			return fmt.Errorf("post hold: %w", err)
		}
		// A lost lease fails here and rolls everything back, the hold too.
		return idempotencyadapters.SetRecoveryPoint(ctx, tx, held, app.RefundRequested, refund.ID())
	})
	if err != nil {
		return domain.Refund{}, fmt.Errorf("payments: create refund: %w", err)
	}
	return refund, nil
}

func (p *Postgres) GetRefund(ctx context.Context, merchant domain.MerchantID, id domain.RefundID) (domain.Refund, error) {
	row, err := paymentsdb.New(p.pool).GetMerchantRefund(ctx, paymentsdb.GetMerchantRefundParams{
		ID:         pgtype.UUID{Bytes: id, Valid: true},
		MerchantID: pgtype.UUID{Bytes: merchant, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Refund{}, app.ErrRefundNotFound
	}
	if err != nil {
		return domain.Refund{}, fmt.Errorf("payments: get refund %s: %w", id, err)
	}
	return restoreRefund(&row)
}

func (p *Postgres) PaidAttempt(ctx context.Context, payment domain.PaymentID) (domain.PaymentAttempt, error) {
	row, err := paymentsdb.New(p.pool).SucceededAttempt(ctx, pgtype.UUID{Bytes: payment, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PaymentAttempt{}, fmt.Errorf("payments: payment %s: %w", payment, app.ErrAttemptNotFound)
	}
	if err != nil {
		return domain.PaymentAttempt{}, fmt.Errorf("payments: paid attempt of %s: %w", payment, err)
	}
	return restoreAttempt(&row)
}

// ApplyRefund takes the locks in the order every path takes them: the payment, the
// refund, then the ledger's accounts.
func (p *Postgres) ApplyRefund(ctx context.Context, id domain.RefundID, u *domain.RefundUpdate, rate fx.Rate, now time.Time) (refund domain.Refund, changed bool, err error) {
	err = postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		q := paymentsdb.New(tx)
		pay, err := q.LockPaymentOfRefund(ctx, pgtype.UUID{Bytes: id, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrRefundNotFound
		}
		if err != nil {
			return fmt.Errorf("lock payment: %w", err)
		}
		row, err := q.LockRefund(ctx, pgtype.UUID{Bytes: id, Valid: true})
		if err != nil {
			return fmt.Errorf("lock refund: %w", err)
		}
		if refund, err = restoreRefund(&row); err != nil {
			return err
		}
		from := refund.Status()
		if changed, err = refund.Apply(u, now); err != nil {
			return fmt.Errorf("refund: %w", err)
		}
		if !changed {
			return nil
		}
		err = q.UpdateRefund(ctx, paymentsdb.UpdateRefundParams{
			ID:         row.ID,
			Status:     string(refund.Status()),
			ExternalID: pgtype.Text{String: refund.ExternalID(), Valid: refund.ExternalID() != ""},
			Reason:     pgtype.Text{String: refund.Reason(), Valid: refund.Reason() != ""},
			UpdatedAt:  pgtype.Timestamptz{Time: refund.UpdatedAt(), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("update refund: %w", err)
		}
		if refund.Status() == from {
			return nil
		}
		switch refund.Status() {
		case domain.RefundSucceeded:
			return completeRefund(ctx, tx, p.sinks.Outbox, &pay, &refund, rate, now)
		case domain.RefundRejected:
			return releaseRefund(ctx, tx, p.sinks.Outbox, &pay, &refund, now)
		case domain.RefundCreated, domain.RefundSent, domain.RefundUnknown:
			return nil
		default:
			return fmt.Errorf("refund has unknown status %q", refund.Status())
		}
	})
	if err != nil {
		return domain.Refund{}, false, fmt.Errorf("payments: apply %s to refund %s: %w", u.Event, id, err)
	}
	return refund, changed, nil
}

// completeRefund posts the money gone back and moves the payment: refunded once the
// refunds that succeeded add up to it, partially refunded before.
func completeRefund(ctx context.Context, tx pgx.Tx, outbox Outbox, pay *paymentsdb.LockPaymentOfRefundRow, refund *domain.Refund, rate fx.Rate, now time.Time) error {
	quote, err := loadQuote(ctx, tx, pay.QuoteID.Bytes)
	if err != nil {
		return err
	}
	accounts, err := refundAccounts(ctx, tx, pay.MerchantID.Bytes, &quote)
	if err != nil {
		return err
	}
	amounts := refund.Amounts()
	t, err := domain.RefundCompletionPosting(refund.ID(), &amounts, rate, &accounts)
	if err != nil {
		return fmt.Errorf("completion: %w", err)
	}
	if _, err := (ledgeradapters.Ledger{}).Post(ctx, tx, t); err != nil {
		return fmt.Errorf("post completion: %w", err)
	}

	succeeded, err := paymentsdb.New(tx).SucceededRefunds(ctx, pay.ID)
	if err != nil {
		return fmt.Errorf("succeeded refunds: %w", err)
	}
	event := domain.RefundPartial
	if succeeded == pay.Amount {
		event = domain.RefundFull
	}
	status, err := domain.ParseStatus(pay.Status)
	if err != nil {
		return fmt.Errorf("stored payment: %w", err)
	}
	to, _, err := domain.Transition(status, event)
	if err != nil {
		return fmt.Errorf("payment: %w", err)
	}
	version, err := setStatus(ctx, tx, pay.ID, to, now)
	if err != nil {
		return err
	}
	return notifyRefund(ctx, tx, outbox, domain.RefundSucceededEvent, refund, pay, to, version, now)
}

// releaseRefund gives the hold back to the merchant; the payment does not change.
func releaseRefund(ctx context.Context, tx pgx.Tx, outbox Outbox, pay *paymentsdb.LockPaymentOfRefundRow, refund *domain.Refund, now time.Time) error {
	quote, err := loadQuote(ctx, tx, pay.QuoteID.Bytes)
	if err != nil {
		return err
	}
	accounts, err := refundAccounts(ctx, tx, pay.MerchantID.Bytes, &quote)
	if err != nil {
		return err
	}
	amounts := refund.Amounts()
	t, err := domain.RefundReleasePosting(refund.ID(), &amounts, &accounts)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if _, err := (ledgeradapters.Ledger{}).Post(ctx, tx, t); err != nil {
		return fmt.Errorf("post release: %w", err)
	}
	status, err := domain.ParseStatus(pay.Status)
	if err != nil {
		return fmt.Errorf("stored payment: %w", err)
	}
	return notifyRefund(ctx, tx, outbox, domain.RefundFailedEvent, refund, pay, status, int(pay.Version), now)
}

func notifyRefund(ctx context.Context, tx pgx.Tx, outbox Outbox, t domain.EventType, refund *domain.Refund, pay *paymentsdb.LockPaymentOfRefundRow, status domain.Status, version int, now time.Time) error {
	currency, err := money.ParseCurrency(pay.Currency)
	if err != nil {
		return fmt.Errorf("stored payment: %w", err)
	}
	id, err := paymentsdb.New(tx).NewEventID(ctx)
	if err != nil {
		return fmt.Errorf("event ID: %w", err)
	}
	e, err := domain.NewRefundEvent(id.Bytes, t, refund.ID(), refund.Amounts().Amount, &domain.PaymentSnapshot{
		ID:       pay.ID.Bytes,
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

// refundAccounts finds the chart of accounts' rows (migrations 00009 and 00017) for the
// quote's currencies, and the merchant's payable.
func refundAccounts(ctx context.Context, tx pgx.Tx, merchant domain.MerchantID, q *fx.Quote) (domain.RefundAccounts, error) {
	payer, base := q.PayerAmount().Currency(), q.Amount().Currency()
	receivables, fxPayer, fxBase := "provider_receivables:"+payer.Code(), "fx:"+payer.Code(), "fx:"+base.Code()
	inFlight, spread, result := "refunds_in_flight:"+base.Code(), "spread_revenue:"+base.Code(), "fx_result:"+base.Code()

	l := ledgeradapters.Ledger{}
	system, err := l.AccountsByCode(ctx, tx, receivables, fxPayer, fxBase, inFlight, spread, result)
	if err != nil {
		return domain.RefundAccounts{}, fmt.Errorf("chart of accounts: %w", err)
	}
	payable, err := l.EnsureAccount(ctx, tx, fmt.Sprintf("merchant_payable:%x:%s", merchant, base.Code()), base, ledger.MayGoNegative)
	if err != nil {
		return domain.RefundAccounts{}, fmt.Errorf("merchant payable: %w", err)
	}
	return domain.RefundAccounts{
		ProviderReceivables: system[receivables].ID(),
		FXPayer:             system[fxPayer].ID(),
		FXMerchant:          system[fxBase].ID(),
		MerchantPayable:     payable.ID(),
		RefundsInFlight:     system[inFlight].ID(),
		SpreadRevenue:       system[spread].ID(),
		FXResult:            system[result].ID(),
	}, nil
}

func restoreRefund(row *paymentsdb.Refund) (domain.Refund, error) {
	status, err := domain.ParseRefundStatus(row.Status)
	if err != nil {
		return domain.Refund{}, fmt.Errorf("payments: stored refund %x: %w", row.ID.Bytes, err)
	}
	currency, err := money.ParseCurrency(row.Currency)
	if err != nil {
		return domain.Refund{}, fmt.Errorf("payments: stored refund %x: %w", row.ID.Bytes, err)
	}
	payer, err := money.ParseCurrency(row.PayerCurrency)
	if err != nil {
		return domain.Refund{}, fmt.Errorf("payments: stored refund %x: %w", row.ID.Bytes, err)
	}
	return domain.RestoreRefund(row.ID.Bytes, row.PaymentID.Bytes, domain.ProviderName(row.Provider), status,
		&domain.RefundAmounts{
			Amount:      money.New(row.Amount, currency),
			PayerAmount: money.New(row.PayerAmount, payer),
			Spread:      money.New(row.Spread, currency),
		},
		row.ExternalID.String, row.Reason.String, row.CreatedAt.Time.UTC(), row.UpdatedAt.Time.UTC()), nil
}
