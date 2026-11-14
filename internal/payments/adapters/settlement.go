package adapters

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
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
// The settleable payments are locked first and the merchant's payable and reserve
// after, the order of every other path; with them locked nothing else can post to
// them, and every change to what is pending (a confirmation, a refund's hold or
// release) posts to the payable, so the balance and the pending sum are read at the
// same point.
//
// Holds due by the cutoff go back to the payable first (decision 26). The available
// amount is then the balance less what is pending: refunds and chargebacks of payments
// already settled come out of it (section 4.7). A negative amount takes what it can
// from the reserve and pays nothing (decision 27); a positive one is paid out less the
// policy's hold, which goes into the reserve. settled is false when nothing was paid or
// the cutoff was already settled.
func (p *Postgres) Settle(ctx context.Context, merchant domain.MerchantID, policy *domain.SettlementPolicy, now time.Time) (s domain.Settlement, settled bool, err error) {
	cutoff := policy.CutoffDate(now)
	before := func(m domain.Method) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: policy.AvailableBefore(m, cutoff), Valid: true}
	}
	pix, boleto, card := before(domain.Pix), before(domain.Boleto), before(domain.Card)
	currency := domain.ChargeCurrency
	merchantID := pgtype.UUID{Bytes: merchant, Valid: true}
	cutoffDate := pgtype.Date{Time: cutoff, Valid: true}

	err = postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		q := paymentsdb.New(tx)
		ids, err := q.LockSettleablePayments(ctx, paymentsdb.LockSettleablePaymentsParams{
			MerchantID: merchantID, PixBefore: pix, BoletoBefore: boleto, CardBefore: card,
		})
		if err != nil {
			return fmt.Errorf("lock payments: %w", err)
		}
		l := ledgeradapters.Ledger{}
		accounts, err := lockMerchantAccounts(ctx, tx, merchant, currency)
		if err != nil {
			return err
		}

		reserve, err := releaseReserve(ctx, tx, merchant, &accounts, cutoff)
		if err != nil {
			return err
		}
		balance, err := l.Balance(ctx, tx, accounts.payable)
		if err != nil {
			return fmt.Errorf("payable balance: %w", err)
		}
		pending, err := q.PendingNet(ctx, paymentsdb.PendingNetParams{
			MerchantID: merchantID, PixBefore: pix, BoletoBefore: boleto, CardBefore: card,
		})
		if err != nil {
			return fmt.Errorf("pending: %w", err)
		}
		available := balance.Amount() - pending
		if available <= 0 {
			return coverWithReserve(ctx, tx, merchant, &accounts, available, reserve)
		}

		hold := policy.ReserveHold(available)
		until := pgtype.Date{}
		if hold > 0 {
			until = pgtype.Date{Time: policy.ReserveUntil(cutoff), Valid: true}
		}
		row, err := q.InsertSettlement(ctx, paymentsdb.InsertSettlementParams{
			MerchantID:   merchantID,
			CutoffDate:   cutoffDate,
			Amount:       available - hold,
			Currency:     currency.Code(),
			Reserved:     hold,
			ReserveUntil: until,
			CreatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Settled already; a release this run posted would be the cutoff's second.
			return errAlreadySettled
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
		amount, held := money.New(row.Amount, currency), money.New(hold, currency)
		t, err := domain.SettlementPosting(id, amount, held, accounts.payable, paid["settlements_paid:"+currency.Code()].ID(), accounts.reserve)
		if err != nil {
			return fmt.Errorf("posting: %w", err)
		}
		if _, err := l.Post(ctx, tx, t); err != nil {
			return fmt.Errorf("post: %w", err)
		}
		s = settlementFromRow(&row, merchant, currency)
		s.Payments = make([]domain.PaymentID, len(ids))
		for i, pid := range ids {
			s.Payments[i] = pid.Bytes
		}
		settled = true
		return nil
	})
	if errors.Is(err, errAlreadySettled) {
		return domain.Settlement{}, false, nil
	}
	if err != nil {
		return domain.Settlement{}, false, fmt.Errorf("payments: settle merchant %x: %w", merchant, err)
	}
	return s, settled, nil
}

var errAlreadySettled = errors.New("payments: cutoff already settled")

func settlementFromRow(r *paymentsdb.Settlement, merchant domain.MerchantID, currency money.Currency) domain.Settlement {
	s := domain.Settlement{
		ID:         r.ID.Bytes,
		Merchant:   merchant,
		CutoffDate: r.CutoffDate.Time,
		Amount:     money.New(r.Amount, currency),
		Reserved:   money.New(r.Reserved, currency),
		Payments:   []domain.PaymentID{},
		CreatedAt:  r.CreatedAt.Time.UTC(),
	}
	if r.ReserveUntil.Valid {
		s.ReserveUntil = r.ReserveUntil.Time
	}
	return s
}

type merchantAccounts struct {
	payable, reserve ledger.AccountID
}

// lockMerchantAccounts creates the merchant's payable and reserve if they are not there
// yet and locks both, in ascending ID order as Post does, so no other path waits on
// them the other way round.
func lockMerchantAccounts(ctx context.Context, tx pgx.Tx, merchant domain.MerchantID, currency money.Currency) (merchantAccounts, error) {
	l := ledgeradapters.Ledger{}
	payable, err := l.EnsureAccount(ctx, tx, payableCode(merchant, currency), currency, ledger.MayGoNegative)
	if err != nil {
		return merchantAccounts{}, fmt.Errorf("merchant payable: %w", err)
	}
	reserve, err := l.EnsureAccount(ctx, tx, reserveCode(merchant, currency), currency, ledger.MustStayNonNegative)
	if err != nil {
		return merchantAccounts{}, fmt.Errorf("merchant reserve: %w", err)
	}
	ids := []ledger.AccountID{payable.ID(), reserve.ID()}
	slices.SortFunc(ids, func(a, b ledger.AccountID) int { return bytes.Compare(a[:], b[:]) })
	for _, id := range ids {
		if _, err := l.LockBalance(ctx, tx, id); err != nil {
			return merchantAccounts{}, fmt.Errorf("lock merchant account: %w", err)
		}
	}
	return merchantAccounts{payable: payable.ID(), reserve: reserve.ID()}, nil
}

// releaseReserve gives the payable the holds due by the cutoff and marks them released,
// and returns what the reserve holds after.
func releaseReserve(ctx context.Context, tx pgx.Tx, merchant domain.MerchantID, a *merchantAccounts, cutoff time.Time) (int64, error) {
	q := paymentsdb.New(tx)
	l := ledgeradapters.Ledger{}
	balance, err := l.Balance(ctx, tx, a.reserve)
	if err != nil {
		return 0, fmt.Errorf("reserve balance: %w", err)
	}
	merchantID, cutoffDate := pgtype.UUID{Bytes: merchant, Valid: true}, pgtype.Date{Time: cutoff, Valid: true}
	holds, err := q.HeldReserves(ctx, paymentsdb.HeldReservesParams{MerchantID: merchantID, CutoffDate: cutoffDate})
	if err != nil {
		return 0, fmt.Errorf("held reserves: %w", err)
	}
	if holds.Due == 0 {
		return balance.Amount(), nil
	}
	if err := q.MarkReservesReleased(ctx, paymentsdb.MarkReservesReleasedParams{MerchantID: merchantID, CutoffDate: cutoffDate}); err != nil {
		return 0, fmt.Errorf("mark reserves released: %w", err)
	}
	release := domain.ReserveRelease(holds.Held, holds.Due, balance.Amount())
	if release == 0 {
		return balance.Amount(), nil
	}
	t, err := domain.ReservePosting(domain.ReserveReleaseReference(merchant, cutoff), money.New(release, balance.Currency()), a.reserve, a.payable)
	if err != nil {
		return 0, fmt.Errorf("release posting: %w", err)
	}
	if _, err := l.Post(ctx, tx, t); err != nil {
		return 0, fmt.Errorf("post release: %w", err)
	}
	return balance.Amount() - release, nil
}

// coverWithReserve pays a negative available amount from the reserve, as far as it
// goes (decision 27); the rest waits for the next money in.
func coverWithReserve(ctx context.Context, tx pgx.Tx, merchant domain.MerchantID, a *merchantAccounts, available, reserve int64) error {
	cover := domain.ReserveCover(available, reserve)
	if cover == 0 {
		return nil
	}
	id, err := paymentsdb.New(tx).NewID(ctx)
	if err != nil {
		return fmt.Errorf("cover ID: %w", err)
	}
	t, err := domain.ReservePosting(domain.ReserveCoverReference(merchant, id.Bytes), money.New(cover, domain.ChargeCurrency), a.reserve, a.payable)
	if err != nil {
		return fmt.Errorf("cover posting: %w", err)
	}
	if _, err := (ledgeradapters.Ledger{}).Post(ctx, tx, t); err != nil {
		return fmt.Errorf("post cover: %w", err)
	}
	return nil
}

func reserveCode(merchant domain.MerchantID, c money.Currency) string {
	return fmt.Sprintf("merchant_reserve:%x:%s", merchant, c.Code())
}

func payableCode(merchant domain.MerchantID, c money.Currency) string {
	return fmt.Sprintf("merchant_payable:%x:%s", merchant, c.Code())
}
