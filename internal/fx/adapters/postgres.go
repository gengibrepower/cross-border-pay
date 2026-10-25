package adapters

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gengibrepower/cross-border-pay/internal/fx/adapters/fxdb"
	"github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

// SaveQuote stores every computed amount of q. It runs on the caller's connection or
// transaction, so the quote commits together with whatever it prices.
func SaveQuote(ctx context.Context, db fxdb.DBTX, q *domain.Quote) (domain.QuoteID, error) {
	id, err := fxdb.New(db).InsertQuote(ctx, fxdb.InsertQuoteParams{
		BaseCurrency:  q.BaseRate().Base().Code(),
		QuoteCurrency: q.BaseRate().Quote().Code(),
		BaseRate:      q.BaseRate().Value(),
		SpreadBps:     int64(q.Pricing().Spread),
		FeeBps:        int64(q.Pricing().Fee),
		Amount:        q.Amount().Amount(),
		PayerAmount:   q.PayerAmount().Amount(),
		Converted:     q.Converted().Amount(),
		Spread:        q.Spread().Amount(),
		Rounding:      q.Rounding().Amount(),
		Fee:           q.Fee().Amount(),
		MerchantNet:   q.MerchantNet().Amount(),
		CreatedAt:     pgtype.Timestamptz{Time: q.CreatedAt(), Valid: true},
		ExpiresAt:     pgtype.Timestamptz{Time: q.ExpiresAt(), Valid: true},
	})
	if err != nil {
		return domain.QuoteID{}, fmt.Errorf("fx: save quote: %w", err)
	}
	return id.Bytes, nil
}

// LoadQuotes returns the stored quotes with these IDs; a missing ID is not an error, it
// is just absent from the map.
func LoadQuotes(ctx context.Context, db fxdb.DBTX, ids []domain.QuoteID) (map[domain.QuoteID]domain.Quote, error) {
	keys := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		keys[i] = pgtype.UUID{Bytes: id, Valid: true}
	}
	rows, err := fxdb.New(db).GetQuotes(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("fx: load quotes: %w", err)
	}
	quotes := make(map[domain.QuoteID]domain.Quote, len(rows))
	for i := range rows {
		q, err := restoreQuote(&rows[i])
		if err != nil {
			return nil, fmt.Errorf("fx: load quote %x: %w", rows[i].ID.Bytes, err)
		}
		quotes[rows[i].ID.Bytes] = q
	}
	return quotes, nil
}

func restoreQuote(row *fxdb.Quote) (domain.Quote, error) {
	base, err := money.ParseCurrency(row.BaseCurrency)
	if err != nil {
		return domain.Quote{}, fmt.Errorf("base currency: %w", err)
	}
	quote, err := money.ParseCurrency(row.QuoteCurrency)
	if err != nil {
		return domain.Quote{}, fmt.Errorf("quote currency: %w", err)
	}
	rate, err := domain.NewRate(base, quote, row.BaseRate)
	if err != nil {
		return domain.Quote{}, fmt.Errorf("base rate: %w", err)
	}
	q, err := domain.RestoreQuote(&domain.StoredQuote{
		Pricing:     domain.Pricing{Spread: domain.BasisPoints(row.SpreadBps), Fee: domain.BasisPoints(row.FeeBps)},
		BaseRate:    rate,
		Amount:      row.Amount,
		PayerAmount: row.PayerAmount,
		Converted:   row.Converted,
		Spread:      row.Spread,
		Rounding:    row.Rounding,
		Fee:         row.Fee,
		MerchantNet: row.MerchantNet,
		CreatedAt:   row.CreatedAt.Time.UTC(),
		ExpiresAt:   row.ExpiresAt.Time.UTC(),
	})
	if err != nil {
		return domain.Quote{}, fmt.Errorf("restore: %w", err)
	}
	return q, nil
}
