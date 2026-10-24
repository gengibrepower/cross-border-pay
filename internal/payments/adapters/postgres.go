package adapters

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	fxadapters "github.com/gengibrepower/cross-border-pay/internal/fx/adapters"
	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

func (p *Postgres) Create(ctx context.Context, d *domain.Draft, q *fx.Quote) (domain.Payment, error) {
	var id pgtype.UUID
	err := postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		quoteID, err := fxadapters.SaveQuote(ctx, tx, q)
		if err != nil {
			return fmt.Errorf("quote: %w", err)
		}
		id, err = paymentsdb.New(tx).InsertPayment(ctx, paymentsdb.InsertPaymentParams{
			MerchantID: pgtype.UUID{Bytes: d.Merchant(), Valid: true},
			QuoteID:    pgtype.UUID{Bytes: quoteID, Valid: true},
			Status:     string(domain.Pending),
			Method:     string(d.Method()),
			Amount:     d.Amount().Amount(),
			Currency:   d.Amount().Currency().Code(),
			Reference:  d.Reference(),
			CreatedAt:  pgtype.Timestamptz{Time: q.CreatedAt(), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("insert payment: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Payment{}, fmt.Errorf("payments: store: %w", err)
	}
	return domain.Restore(id.Bytes, d.Merchant(), domain.Pending, d.Method(), d.Amount(), d.Reference(), q.CreatedAt()), nil
}
