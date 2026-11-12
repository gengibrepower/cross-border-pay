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
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

// Balance splits the merchant's payable as Settle does at now: what is pending, and the
// rest available. Both are read in one REPEATABLE READ snapshot, without the payable's
// lock: a read never waits on a confirmation or blocks a settlement, and the two sums
// still agree, since every change to what is pending posts to the payable in the same
// transaction. A merchant with no confirmed payment has no payable yet: all zero.
func (p *Postgres) Balance(ctx context.Context, merchant domain.MerchantID, policy *domain.SettlementPolicy, now time.Time) (domain.Balance, error) {
	cutoff := policy.CutoffDate(now)
	before := func(m domain.Method) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: policy.AvailableBefore(m, cutoff), Valid: true}
	}
	currency := domain.ChargeCurrency
	var b domain.Balance
	err := postgres.WithTx(ctx, p.pool, pgx.RepeatableRead, func(ctx context.Context, tx pgx.Tx) error {
		l := ledgeradapters.Ledger{}
		code := payableCode(merchant, currency)
		accounts, err := l.AccountsByCode(ctx, tx, code)
		if errors.Is(err, ledger.ErrUnknownAccount) {
			b = domain.Balance{Available: money.Zero(currency), Pending: money.Zero(currency), Reserved: money.Zero(currency)}
			return nil
		}
		if err != nil {
			return fmt.Errorf("merchant payable: %w", err)
		}
		balance, err := l.Balance(ctx, tx, accounts[code].ID())
		if err != nil {
			return fmt.Errorf("payable balance: %w", err)
		}
		pending, err := paymentsdb.New(tx).PendingNet(ctx, paymentsdb.PendingNetParams{
			MerchantID: pgtype.UUID{Bytes: merchant, Valid: true},
			PixBefore:  before(domain.Pix), BoletoBefore: before(domain.Boleto), CardBefore: before(domain.Card),
		})
		if err != nil {
			return fmt.Errorf("pending: %w", err)
		}
		b = domain.Balance{
			Available: money.New(balance.Amount()-pending, currency),
			Pending:   money.New(pending, currency),
			Reserved:  money.Zero(currency),
		}
		return nil
	})
	if err != nil {
		return domain.Balance{}, fmt.Errorf("payments: balance of merchant %x: %w", merchant, err)
	}
	return b, nil
}

// ListSettlements reads the page and then its payments, in one snapshot so that a
// settlement committed between the two reads cannot show without its payments.
func (p *Postgres) ListSettlements(ctx context.Context, merchant domain.MerchantID, q app.SettlementQuery) ([]domain.Settlement, error) {
	var out []domain.Settlement
	err := postgres.WithTx(ctx, p.pool, pgx.RepeatableRead, func(ctx context.Context, tx pgx.Tx) error {
		db := paymentsdb.New(tx)
		rows, err := db.ListSettlements(ctx, paymentsdb.ListSettlementsParams{
			MerchantID: pgtype.UUID{Bytes: merchant, Valid: true},
			Before:     pgtype.Date{Time: q.Before, Valid: !q.Before.IsZero()},
			MaxRows:    int64(q.Limit),
		})
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		ids := make([]pgtype.UUID, len(rows))
		out = make([]domain.Settlement, len(rows))
		index := make(map[[16]byte]int, len(rows))
		for i := range rows {
			r := &rows[i]
			currency, err := money.ParseCurrency(r.Currency)
			if err != nil {
				return fmt.Errorf("settlement %x: %w", r.ID.Bytes, err)
			}
			ids[i] = r.ID
			index[r.ID.Bytes] = i
			out[i] = domain.Settlement{
				ID:         r.ID.Bytes,
				Merchant:   merchant,
				CutoffDate: r.CutoffDate.Time,
				Amount:     money.New(r.Amount, currency),
				Payments:   []domain.PaymentID{},
				CreatedAt:  r.CreatedAt.Time.UTC(),
			}
		}
		if len(rows) == 0 {
			return nil
		}
		included, err := db.SettlementPayments(ctx, ids)
		if err != nil {
			return fmt.Errorf("payments: %w", err)
		}
		for _, r := range included {
			s := &out[index[r.SettlementID.Bytes]]
			s.Payments = append(s.Payments, r.ID.Bytes)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("payments: settlements of merchant %x: %w", merchant, err)
	}
	return out, nil
}
