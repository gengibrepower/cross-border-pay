package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	ledgeradapters "github.com/gengibrepower/cross-border-pay/internal/ledger/adapters"
	ledger "github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

func (p *Postgres) SettlementCandidates(ctx context.Context, cutoff time.Time, after domain.MerchantID, limit int) ([]domain.MerchantID, error) {
	rows, err := paymentsdb.New(p.pool).SettlementCandidates(ctx, paymentsdb.SettlementCandidatesParams{
		After:      pgtype.UUID{Bytes: after, Valid: true},
		CutoffDate: pgtype.Date{Time: cutoff, Valid: true},
		MaxRows:    int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("payments: settlement candidates: %w", err)
	}
	ids := make([]domain.MerchantID, len(rows))
	for i, r := range rows {
		ids[i] = r.Bytes
	}
	return ids, nil
}

// Settle pays the merchant what is available at the cutoff of now, in one transaction.
// The settleable payments are locked first and the payable after, the order of every
// other path; with the payable locked nothing else can post to it, and every change to
// what is pending (a confirmation, a refund's hold or release) posts to it, so the
// balance and the pending sum are read at the same point. The available amount is the
// balance less what is pending: refunds of payments already settled come out of it
// (section 4.7), and a negative amount pays nothing and waits (decision 27, F8).
// settled is false when nothing was due or the cutoff was already settled.
func (p *Postgres) Settle(ctx context.Context, merchant domain.MerchantID, policy *domain.SettlementPolicy, now time.Time) (s domain.Settlement, settled bool, err error) {
	cutoff := policy.CutoffDate(now)
	before := func(m domain.Method) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: policy.AvailableBefore(m, cutoff), Valid: true}
	}
	pix, boleto, card := before(domain.Pix), before(domain.Boleto), before(domain.Card)
	currency := domain.ChargeCurrency

	err = postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		q := paymentsdb.New(tx)
		ids, err := q.LockSettleablePayments(ctx, paymentsdb.LockSettleablePaymentsParams{
			MerchantID: pgtype.UUID{Bytes: merchant, Valid: true}, PixBefore: pix, BoletoBefore: boleto, CardBefore: card,
		})
		if err != nil {
			return fmt.Errorf("lock payments: %w", err)
		}
		l := ledgeradapters.Ledger{}
		payable, err := l.EnsureAccount(ctx, tx, payableCode(merchant, currency), currency, ledger.MayGoNegative)
		if err != nil {
			return fmt.Errorf("merchant payable: %w", err)
		}
		balance, err := l.LockBalance(ctx, tx, payable.ID())
		if err != nil {
			return fmt.Errorf("payable balance: %w", err)
		}
		pending, err := q.PendingNet(ctx, paymentsdb.PendingNetParams{
			MerchantID: pgtype.UUID{Bytes: merchant, Valid: true}, PixBefore: pix, BoletoBefore: boleto, CardBefore: card,
		})
		if err != nil {
			return fmt.Errorf("pending: %w", err)
		}
		available := balance.Amount() - pending
		if available <= 0 {
			return nil
		}

		row, err := q.InsertSettlement(ctx, paymentsdb.InsertSettlementParams{
			MerchantID: pgtype.UUID{Bytes: merchant, Valid: true},
			CutoffDate: pgtype.Date{Time: cutoff, Valid: true},
			Amount:     available,
			Currency:   currency.Code(),
			CreatedAt:  pgtype.Timestamptz{Time: now, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert settlement: %w", err)
		}
		if err := q.MarkSettled(ctx, paymentsdb.MarkSettledParams{SettlementID: row.ID, Ids: ids}); err != nil {
			return fmt.Errorf("mark settled: %w", err)
		}
		paid, err := l.AccountsByCode(ctx, tx, "settlements_paid:"+currency.Code())
		if err != nil {
			return fmt.Errorf("chart of accounts: %w", err)
		}
		id := domain.SettlementID(row.ID.Bytes)
		amount := money.New(available, currency)
		t, err := domain.SettlementPosting(id, amount, payable.ID(), paid["settlements_paid:"+currency.Code()].ID())
		if err != nil {
			return fmt.Errorf("posting: %w", err)
		}
		if _, err := l.Post(ctx, tx, t); err != nil {
			return fmt.Errorf("post: %w", err)
		}
		s = domain.Settlement{
			ID: id, Merchant: merchant, CutoffDate: cutoff, Amount: amount,
			Payments: make([]domain.PaymentID, len(ids)), CreatedAt: row.CreatedAt.Time.UTC(),
		}
		for i, pid := range ids {
			s.Payments[i] = pid.Bytes
		}
		settled = true
		return nil
	})
	if err != nil {
		return domain.Settlement{}, false, fmt.Errorf("payments: settle merchant %x: %w", merchant, err)
	}
	return s, settled, nil
}

func payableCode(merchant domain.MerchantID, c money.Currency) string {
	return fmt.Sprintf("merchant_payable:%x:%s", merchant, c.Code())
}
