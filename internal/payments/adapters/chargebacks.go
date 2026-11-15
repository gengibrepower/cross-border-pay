package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	ledgeradapters "github.com/gengibrepower/cross-border-pay/internal/ledger/adapters"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
)

// HandleCardSimEvent is HandlePixSimEvent for chargebacks. It returns
// app.ErrEventEarly for a chargeback of a capture not confirmed yet, or a close whose
// opening was not applied yet.
func HandleCardSimEvent(ctx context.Context, tx pgx.Tx, sinks *Sinks, payload []byte, now time.Time) (subject string, err error) {
	var e cardsim.Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return "", fmt.Errorf("%w: %w", app.ErrEventDivergent, err)
	}
	var apply func(context.Context, pgx.Tx, *Sinks, *paymentsdb.LockPaymentOfAttemptRow, *cardsim.Chargeback, time.Time) error
	switch e.Type {
	case cardsim.ChargebackOpened:
		apply = openChargeback
	case cardsim.ChargebackClosed:
		apply = closeChargeback
	default:
		return "", fmt.Errorf("%w: %q", app.ErrUnknownEventType, e.Type)
	}
	id, err := domain.ParseAttemptID(e.Chargeback.AuthorizationKey)
	if err != nil {
		return "", fmt.Errorf("%w: %w", app.ErrEventDivergent, err)
	}
	subject = id.String()
	row, err := lockPayment(ctx, paymentsdb.New(tx), id)
	if err != nil {
		return subject, fmt.Errorf("payments: %s: %w", id, err)
	}
	if row.AttemptProvider != string(CardSimName) {
		return subject, fmt.Errorf("%w: attempt %s is held by %s, not %s", app.ErrEventDivergent, id, row.AttemptProvider, CardSimName)
	}
	// In a savepoint, as applySaved: a divergent event commits with its status only.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return subject, fmt.Errorf("savepoint: %w", err)
	}
	if err := apply(ctx, sp, sinks, &row, &e.Chargeback, now); err != nil {
		if rerr := sp.Rollback(ctx); rerr != nil {
			return subject, errors.Join(err, fmt.Errorf("rollback to savepoint: %w", rerr))
		}
		if errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrChargebackExceedsPayment) ||
			errors.Is(err, domain.ErrChargebackClosed) || errors.Is(err, domain.ErrInvalidReason) ||
			errors.Is(err, money.ErrCurrencyMismatch) || errors.Is(err, domain.ErrInvalidAmount) || errors.Is(err, errUnpostable) {
			return subject, fmt.Errorf("%w: %w", app.ErrEventDivergent, err)
		}
		return subject, fmt.Errorf("payments: chargeback %s of %s: %w", e.Chargeback.ID, id, err)
	}
	if err := sp.Commit(ctx); err != nil {
		return subject, fmt.Errorf("release savepoint: %w", err)
	}
	return subject, nil
}

// openChargeback: a refunded payment is charged back too (decision 7), and a pending
// one is a capture still being confirmed, so the event is early and retried.
func openChargeback(ctx context.Context, tx pgx.Tx, sinks *Sinks, pay *paymentsdb.LockPaymentOfAttemptRow, cb *cardsim.Chargeback, now time.Time) error {
	status, err := domain.ParseStatus(pay.Status)
	if err != nil {
		return fmt.Errorf("stored payment: %w", err)
	}
	if status == domain.Pending {
		return fmt.Errorf("%w: payment %x is not confirmed yet", app.ErrEventEarly, pay.PaymentID.Bytes)
	}
	q := paymentsdb.New(tx)
	// Stored by an earlier delivery, under another event ID: looked up before pricing,
	// which would count it twice.
	_, err = q.LockChargebackByExternalID(ctx, paymentsdb.LockChargebackByExternalIDParams{Provider: string(CardSimName), ExternalID: cb.ID})
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock chargeback: %w", err)
	}
	to, _, err := domain.Transition(status, domain.Chargeback)
	if err != nil {
		return fmt.Errorf("payment: %w", err)
	}
	reason, err := domain.NewReason(cb.Reason)
	if err != nil {
		return fmt.Errorf("chargeback %s: %w", cb.ID, err)
	}
	quote, err := loadQuote(ctx, tx, pay.QuoteID.Bytes)
	if err != nil {
		return err
	}
	before, err := q.ChargebacksOfPayment(ctx, pay.PaymentID)
	if err != nil {
		return fmt.Errorf("chargebacks of payment: %w", err)
	}
	payer, base := quote.PayerAmount().Currency(), quote.Amount().Currency()
	amounts, err := domain.PriceChargeback(&quote, &domain.ChargebackAmounts{
		PayerAmount: money.New(before.PayerAmount, payer),
		Amount:      money.New(before.Amount, base),
		Spread:      money.New(before.Spread, base),
	}, cb.Amount)
	if err != nil {
		return fmt.Errorf("price chargeback %s: %w", cb.ID, err)
	}
	row, err := q.InsertChargeback(ctx, paymentsdb.InsertChargebackParams{
		PaymentID: pay.PaymentID, MerchantID: pay.MerchantID, Provider: string(CardSimName), ExternalID: cb.ID,
		Reason: reason, PayerAmount: amounts.PayerAmount.Amount(), PayerCurrency: payer.Code(),
		Amount: amounts.Amount.Amount(), Spread: amounts.Spread.Amount(), Currency: base.Code(),
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // stored since the lookup: the payment's lock makes this unreachable
	}
	if err != nil {
		return fmt.Errorf("insert chargeback: %w", err)
	}
	c, err := restoreChargeback(&row)
	if err != nil {
		return err
	}
	if err := postChargeback(ctx, tx, sinks, &c, domain.ChargebackDebit, now); err != nil {
		return err
	}
	return notifyChargeback(ctx, tx, sinks.Outbox, domain.ChargebackOpenedEvent, pay.PaymentID, pay.MerchantID, to, &c, now)
}

func closeChargeback(ctx context.Context, tx pgx.Tx, sinks *Sinks, pay *paymentsdb.LockPaymentOfAttemptRow, cb *cardsim.Chargeback, now time.Time) error {
	var to domain.ChargebackStatus
	switch cb.Status {
	case cardsim.Won:
		to = domain.Won
	case cardsim.Lost:
		to = domain.Lost
	case cardsim.NeedsResponse, cardsim.UnderReview:
		return fmt.Errorf("%w: chargeback.closed with status %q", domain.ErrChargebackClosed, cb.Status)
	default:
		return fmt.Errorf("%w: chargeback.closed with status %q", domain.ErrChargebackClosed, cb.Status)
	}
	row, err := paymentsdb.New(tx).LockChargebackByExternalID(ctx, paymentsdb.LockChargebackByExternalIDParams{
		Provider: string(CardSimName), ExternalID: cb.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: chargeback %s is not open yet", app.ErrEventEarly, cb.ID)
	}
	if err != nil {
		return fmt.Errorf("lock chargeback: %w", err)
	}
	status, err := domain.ParseStatus(pay.Status)
	if err != nil {
		return fmt.Errorf("stored payment: %w", err)
	}
	_, _, err = moveChargeback(ctx, tx, sinks, pay.PaymentID, pay.MerchantID, status, &row, to, "", now)
	return err
}

// moveChargeback locks the payment first and the chargeback after.
func moveChargeback(ctx context.Context, tx pgx.Tx, sinks *Sinks, payment, merchant pgtype.UUID, status domain.Status,
	row *paymentsdb.Chargeback, to domain.ChargebackStatus, evidence string, now time.Time,
) (domain.ChargebackCase, bool, error) {
	c, err := restoreChargeback(row)
	if err != nil {
		return domain.ChargebackCase{}, false, err
	}
	next, changed, err := domain.MoveChargeback(c.Status, to)
	if err != nil {
		return c, false, fmt.Errorf("chargeback %s: %w", c.ID, err)
	}
	if !changed {
		return c, false, nil
	}
	c.Status, c.UpdatedAt = next, now
	if evidence != "" {
		c.Evidence = evidence
	}
	closedAt := pgtype.Timestamptz{}
	if next.Closed() {
		c.ClosedAt = now
		closedAt = pgtype.Timestamptz{Time: now, Valid: true}
	}
	if err := paymentsdb.New(tx).UpdateChargeback(ctx, paymentsdb.UpdateChargebackParams{
		ID: row.ID, Status: string(next), Evidence: pgtype.Text{String: evidence, Valid: evidence != ""},
		UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true}, ClosedAt: closedAt,
	}); err != nil {
		return domain.ChargebackCase{}, false, fmt.Errorf("update chargeback: %w", err)
	}
	switch next {
	case domain.Won:
		if err := postChargeback(ctx, tx, sinks, &c, domain.ChargebackReversal, now); err != nil {
			return domain.ChargebackCase{}, false, err
		}
		err = notifyChargeback(ctx, tx, sinks.Outbox, domain.ChargebackWonEvent, payment, merchant, status, &c, now)
	case domain.Lost:
		err = notifyChargeback(ctx, tx, sinks.Outbox, domain.ChargebackLostEvent, payment, merchant, status, &c, now)
	case domain.NeedsResponse, domain.UnderReview:
	}
	if err != nil {
		return domain.ChargebackCase{}, false, err
	}
	return c, true, nil
}

func postChargeback(ctx context.Context, tx pgx.Tx, sinks *Sinks, c *domain.ChargebackCase, step domain.ChargebackStep, now time.Time) error {
	if sinks.Rates == nil {
		return errors.New("payments: no rate source to post a chargeback with")
	}
	payer, base := c.Amounts.PayerAmount.Currency(), c.Amounts.Amount.Currency()
	rate, err := sinks.Rates.Rate(ctx, base, payer)
	if err != nil {
		return fmt.Errorf("rate at %s: %w", now.Format(time.RFC3339), err)
	}
	accounts, err := chargebackAccounts(ctx, tx, c.Merchant, payer, base)
	if err != nil {
		return err
	}
	post := domain.ChargebackDebitPosting
	if step == domain.ChargebackReversal {
		post = domain.ChargebackReversalPosting
	}
	t, err := post(c.ID, &c.Amounts, rate, &accounts)
	if err != nil {
		return fmt.Errorf("chargeback %s: %w", step, err)
	}
	_, err = (ledgeradapters.Ledger{}).Post(ctx, tx, t)
	if errors.Is(err, ledger.ErrNegativeBalance) {
		return fmt.Errorf("%w: chargeback %s: %w", errUnpostable, step, err)
	}
	if err != nil {
		return fmt.Errorf("post chargeback %s: %w", step, err)
	}
	return nil
}

// notifyChargeback bumps the payment's version, so the event orders with its others.
func notifyChargeback(ctx context.Context, tx pgx.Tx, outbox Outbox, t domain.EventType, payment, merchant pgtype.UUID, status domain.Status, c *domain.ChargebackCase, now time.Time) error {
	version, err := setStatus(ctx, tx, payment, status, now)
	if err != nil {
		return err
	}
	id, err := paymentsdb.New(tx).NewEventID(ctx)
	if err != nil {
		return fmt.Errorf("event ID: %w", err)
	}
	e, err := domain.NewChargebackEvent(id.Bytes, t, c, &domain.PaymentSnapshot{
		ID: payment.Bytes, Merchant: merchant.Bytes, Version: version, Status: status,
	}, now)
	if err != nil {
		return fmt.Errorf("event: %w", err)
	}
	if err := outbox.Append(ctx, tx, &e); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	return nil
}

func chargebackAccounts(ctx context.Context, tx pgx.Tx, merchant domain.MerchantID, payer, base money.Currency) (domain.ChargebackAccounts, error) {
	receivables, fxPayer, fxBase := "provider_receivables:"+payer.Code(), "fx:"+payer.Code(), "fx:"+base.Code()
	spread, result := "spread_revenue:"+base.Code(), "fx_result:"+base.Code()
	l := ledgeradapters.Ledger{}
	system, err := l.AccountsByCode(ctx, tx, receivables, fxPayer, fxBase, spread, result)
	if err != nil {
		return domain.ChargebackAccounts{}, fmt.Errorf("chart of accounts: %w", err)
	}
	payable, err := l.EnsureAccount(ctx, tx, payableCode(merchant, base), base, ledger.MayGoNegative)
	if err != nil {
		return domain.ChargebackAccounts{}, fmt.Errorf("merchant payable: %w", err)
	}
	return domain.ChargebackAccounts{
		ProviderReceivables: system[receivables].ID(),
		FXPayer:             system[fxPayer].ID(),
		FXMerchant:          system[fxBase].ID(),
		MerchantPayable:     payable.ID(),
		SpreadRevenue:       system[spread].ID(),
		FXResult:            system[result].ID(),
	}, nil
}

func (p *Postgres) GetChargeback(ctx context.Context, merchant domain.MerchantID, id domain.ChargebackID) (domain.ChargebackCase, error) {
	row, err := paymentsdb.New(p.pool).GetChargeback(ctx, paymentsdb.GetChargebackParams{
		ID: pgtype.UUID{Bytes: id, Valid: true}, MerchantID: pgtype.UUID{Bytes: merchant, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ChargebackCase{}, app.ErrChargebackNotFound
	}
	if err != nil {
		return domain.ChargebackCase{}, fmt.Errorf("payments: get chargeback: %w", err)
	}
	return restoreChargeback(&row)
}

// AnswerChargeback locks as an event would: the payment first, then the chargeback.
func (p *Postgres) AnswerChargeback(ctx context.Context, merchant domain.MerchantID, id domain.ChargebackID, to domain.ChargebackStatus, evidence string, now time.Time) (domain.ChargebackCase, error) {
	var c domain.ChargebackCase
	err := postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		q := paymentsdb.New(tx)
		pay, err := q.LockPaymentOfChargeback(ctx, paymentsdb.LockPaymentOfChargebackParams{
			ID: pgtype.UUID{Bytes: id, Valid: true}, MerchantID: pgtype.UUID{Bytes: merchant, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrChargebackNotFound
		}
		if err != nil {
			return fmt.Errorf("lock payment: %w", err)
		}
		row, err := q.LockChargeback(ctx, pgtype.UUID{Bytes: id, Valid: true})
		if err != nil {
			return fmt.Errorf("lock chargeback: %w", err)
		}
		status, err := domain.ParseStatus(pay.Status)
		if err != nil {
			return fmt.Errorf("stored payment: %w", err)
		}
		c, _, err = moveChargeback(ctx, tx, &p.sinks, pay.PaymentID, pay.MerchantID, status, &row, to, evidence, now)
		return err
	})
	if err != nil {
		return domain.ChargebackCase{}, fmt.Errorf("payments: answer chargeback %s: %w", id, err)
	}
	return c, nil
}

func restoreChargeback(row *paymentsdb.Chargeback) (domain.ChargebackCase, error) {
	status, err := domain.ParseChargebackStatus(row.Status)
	if err != nil {
		return domain.ChargebackCase{}, fmt.Errorf("payments: stored chargeback %x: %w", row.ID.Bytes, err)
	}
	payer, err := money.ParseCurrency(row.PayerCurrency)
	if err != nil {
		return domain.ChargebackCase{}, fmt.Errorf("payments: stored chargeback %x: %w", row.ID.Bytes, err)
	}
	base, err := money.ParseCurrency(row.Currency)
	if err != nil {
		return domain.ChargebackCase{}, fmt.Errorf("payments: stored chargeback %x: %w", row.ID.Bytes, err)
	}
	c := domain.ChargebackCase{
		ID: row.ID.Bytes, Payment: row.PaymentID.Bytes, Merchant: row.MerchantID.Bytes,
		Provider: domain.ProviderName(row.Provider), ExternalID: row.ExternalID,
		Reason: row.Reason, Status: status, Evidence: row.Evidence.String,
		Amounts: domain.ChargebackAmounts{
			PayerAmount: money.New(row.PayerAmount, payer),
			Amount:      money.New(row.Amount, base),
			Spread:      money.New(row.Spread, base),
		},
		CreatedAt: row.CreatedAt.Time.UTC(), UpdatedAt: row.UpdatedAt.Time.UTC(),
	}
	if row.ClosedAt.Valid {
		c.ClosedAt = row.ClosedAt.Time.UTC()
	}
	return c, nil
}
