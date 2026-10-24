package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/adapters/merchantsdb"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

func (p *Postgres) CreateMerchantWithKey(ctx context.Context, name string, key *domain.NewKey) (domain.MerchantID, error) {
	var id pgtype.UUID
	err := postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		q := merchantsdb.New(tx)
		var err error
		if id, err = q.CreateMerchant(ctx, name); err != nil {
			return fmt.Errorf("insert merchant: %w", err)
		}
		if err := q.CreateAPIKey(ctx, merchantsdb.CreateAPIKeyParams{
			MerchantID: id,
			Mode:       string(key.Mode),
			Lookup:     key.Lookup,
			KeyHash:    key.Hash[:],
		}); err != nil {
			return fmt.Errorf("insert api key: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.MerchantID{}, fmt.Errorf("merchants: %w", err)
	}
	return id.Bytes, nil
}

func (p *Postgres) FindActiveKey(ctx context.Context, lookup string) (app.StoredKey, error) {
	row, err := merchantsdb.New(p.pool).FindActiveAPIKey(ctx, lookup)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.StoredKey{}, app.ErrKeyNotFound
	}
	if err != nil {
		return app.StoredKey{}, fmt.Errorf("merchants: find key: %w", err)
	}
	mode, err := domain.ParseMode(row.Mode)
	if err != nil {
		return app.StoredKey{}, fmt.Errorf("merchants: stored key: %w", err)
	}
	return app.StoredKey{MerchantID: row.MerchantID.Bytes, Mode: mode, Hash: row.KeyHash}, nil
}

func (p *Postgres) RevokeKey(ctx context.Context, lookup string) error {
	n, err := merchantsdb.New(p.pool).RevokeAPIKey(ctx, lookup)
	if err != nil {
		return fmt.Errorf("merchants: revoke key: %w", err)
	}
	if n == 0 {
		return app.ErrKeyNotFound
	}
	return nil
}
