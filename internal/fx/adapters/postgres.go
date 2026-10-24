package adapters

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gengibrepower/cross-border-pay/internal/fx/adapters/fxdb"
	"github.com/gengibrepower/cross-border-pay/internal/fx/domain"
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
