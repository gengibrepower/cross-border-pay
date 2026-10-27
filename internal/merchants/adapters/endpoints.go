package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/adapters/merchantsdb"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

func (p *Postgres) UpdateEndpoint(ctx context.Context, merchant domain.MerchantID, change func(*domain.Endpoint) (domain.Endpoint, error)) (domain.Endpoint, error) {
	var saved domain.Endpoint
	id := pgtype.UUID{Bytes: merchant, Valid: true}
	err := postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		q := merchantsdb.New(tx)
		if _, err := q.LockMerchant(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return app.ErrMerchantNotFound
		} else if err != nil {
			return fmt.Errorf("lock merchant: %w", err)
		}
		var current *domain.Endpoint
		row, err := q.GetWebhookEndpoint(ctx, id)
		switch {
		case err == nil:
			e := restoreEndpoint(&row)
			current = &e
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("get endpoint: %w", err)
		}
		next, err := change(current)
		if err != nil {
			return err
		}
		err = q.SaveWebhookEndpoint(ctx, merchantsdb.SaveWebhookEndpointParams{
			MerchantID:            id,
			Url:                   next.URL,
			SecretVersion:         int32(next.Version),                                                 //nolint:gosec // G115: versions grow by one per rotation, far from 2^31.
			PreviousSecretVersion: pgtype.Int4{Int32: int32(next.Previous), Valid: next.Previous != 0}, //nolint:gosec // G115: as above.
			RotatedAt:             pgtype.Timestamptz{Time: next.RotatedAt, Valid: !next.RotatedAt.IsZero()},
			CreatedAt:             pgtype.Timestamptz{Time: next.CreatedAt, Valid: true},
			UpdatedAt:             pgtype.Timestamptz{Time: next.UpdatedAt, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("save endpoint: %w", err)
		}
		saved = next
		return nil
	})
	if err != nil {
		return domain.Endpoint{}, fmt.Errorf("merchants: %w", err)
	}
	return saved, nil
}

// Endpoint reads the merchant's endpoint outside any transaction: the delivery worker
// only needs where to post and which secrets sign, as of now.
func (p *Postgres) Endpoint(ctx context.Context, merchant domain.MerchantID) (domain.Endpoint, error) {
	row, err := merchantsdb.New(p.pool).GetWebhookEndpoint(ctx, pgtype.UUID{Bytes: merchant, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Endpoint{}, app.ErrEndpointNotFound
	}
	if err != nil {
		return domain.Endpoint{}, fmt.Errorf("merchants: get endpoint: %w", err)
	}
	return restoreEndpoint(&row), nil
}

func restoreEndpoint(row *merchantsdb.GetWebhookEndpointRow) domain.Endpoint {
	return domain.Endpoint{
		Merchant:  row.MerchantID.Bytes,
		URL:       row.Url,
		Version:   domain.SecretVersion(row.SecretVersion),
		Previous:  domain.SecretVersion(row.PreviousSecretVersion.Int32),
		RotatedAt: row.RotatedAt.Time,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
}
