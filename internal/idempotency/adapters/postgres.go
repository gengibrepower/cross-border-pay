package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/idempotency/adapters/idempotencydb"
	"github.com/gengibrepower/cross-border-pay/internal/idempotency/app"
	"github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

// A key released between our INSERT and our SELECT is gone again; a few rounds
// always settle it, because each one either inserts or finds a row.
const maxAcquireRounds = 3

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

func uuid(b [16]byte) pgtype.UUID { return pgtype.UUID{Bytes: b, Valid: true} }

func timestamptz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func (p *Postgres) Acquire(ctx context.Context, req *domain.Request, token domain.LeaseToken, now time.Time) (app.Acquired, error) {
	var acquired app.Acquired
	err := postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		acquired, err = acquire(ctx, idempotencydb.New(tx), req, token, now)
		return err
	})
	if err != nil {
		return app.Acquired{}, fmt.Errorf("idempotency: acquire: %w", err)
	}
	return acquired, nil
}

func acquire(ctx context.Context, q *idempotencydb.Queries, req *domain.Request, token domain.LeaseToken, now time.Time) (app.Acquired, error) {
	held := &domain.Held{Merchant: req.Merchant, Key: req.Key, Token: token, RecoveryPoint: domain.Started}
	until := timestamptz(now.Add(domain.Lease))
	for range maxAcquireRounds {
		// Under READ COMMITTED a concurrent INSERT of the same key makes this one wait
		// for it to commit, then do nothing: exactly one of them gets the row.
		inserted, err := q.InsertKey(ctx, idempotencydb.InsertKeyParams{
			MerchantID:  uuid(req.Merchant),
			Key:         req.Key,
			RequestHash: req.Hash[:],
			LockToken:   token[:],
			LockedUntil: until,
			CreatedAt:   timestamptz(now),
		})
		if err != nil {
			return app.Acquired{}, fmt.Errorf("insert: %w", err)
		}
		if inserted == 1 {
			return app.Acquired{Held: held}, nil
		}

		row, err := q.LockKey(ctx, idempotencydb.LockKeyParams{MerchantID: uuid(req.Merchant), Key: req.Key})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return app.Acquired{}, fmt.Errorf("lock: %w", err)
		}
		return decide(ctx, q, &row, req, held, now)
	}
	return app.Acquired{}, fmt.Errorf("key kept vanishing after %d rounds", maxAcquireRounds)
}

func decide(ctx context.Context, q *idempotencydb.Queries, row *idempotencydb.IdempotencyKey, req *domain.Request, held *domain.Held, now time.Time) (app.Acquired, error) {
	status := domain.Status(row.Status)
	if status != domain.Processing && status != domain.Completed {
		return app.Acquired{}, fmt.Errorf("unknown status %q", row.Status)
	}
	record := domain.Record{
		Hash:        domain.RequestHash(row.RequestHash),
		Status:      status,
		LockedUntil: row.LockedUntil.Time,
		CreatedAt:   row.CreatedAt.Time,
	}
	until := timestamptz(now.Add(domain.Lease))

	switch domain.Decide(&record, req.Hash, now) {
	case domain.Restart:
		err := q.RestartKey(ctx, idempotencydb.RestartKeyParams{
			MerchantID:  uuid(req.Merchant),
			Key:         req.Key,
			RequestHash: req.Hash[:],
			LockToken:   held.Token[:],
			LockedUntil: until,
			CreatedAt:   timestamptz(now),
		})
		if err != nil {
			return app.Acquired{}, fmt.Errorf("restart: %w", err)
		}
		return app.Acquired{Held: held}, nil
	case domain.Resume:
		err := q.ResumeKey(ctx, idempotencydb.ResumeKeyParams{
			MerchantID:  uuid(req.Merchant),
			Key:         req.Key,
			LockToken:   held.Token[:],
			LockedUntil: until,
		})
		if err != nil {
			return app.Acquired{}, fmt.Errorf("resume: %w", err)
		}
		held.RecoveryPoint = domain.RecoveryPoint(row.RecoveryPoint)
		held.ResourceID = row.ResourceID.Bytes
		return app.Acquired{Held: held}, nil
	case domain.Replay:
		resp, err := storedResponse(row)
		if err != nil {
			return app.Acquired{}, err
		}
		return app.Acquired{Replay: resp}, nil
	case domain.InProgress:
		return app.Acquired{}, app.ErrInProgress
	case domain.Reused:
		return app.Acquired{}, app.ErrKeyReused
	default:
		return app.Acquired{}, errors.New("unknown outcome")
	}
}

func storedResponse(row *idempotencydb.IdempotencyKey) (*domain.Response, error) {
	var headers map[string]string
	if err := json.Unmarshal(row.ResponseHeaders, &headers); err != nil {
		return nil, fmt.Errorf("stored headers: %w", err)
	}
	return &domain.Response{Status: int(row.ResponseCode.Int32), Headers: headers, Body: row.ResponseBody}, nil
}

func (p *Postgres) Complete(ctx context.Context, held *domain.Held, resp *domain.Response) error {
	headers, err := json.Marshal(resp.Headers)
	if err != nil {
		return fmt.Errorf("idempotency: headers: %w", err)
	}
	body := resp.Body
	if body == nil {
		body = []byte{}
	}
	n, err := idempotencydb.New(p.pool).CompleteKey(ctx, idempotencydb.CompleteKeyParams{
		MerchantID:      uuid(held.Merchant),
		Key:             held.Key,
		LockToken:       held.Token[:],
		ResponseCode:    int64(resp.Status),
		ResponseHeaders: headers,
		ResponseBody:    body,
	})
	if err != nil {
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	if n == 0 {
		return app.ErrLeaseLost
	}
	return nil
}

func (p *Postgres) Fail(ctx context.Context, held *domain.Held, now time.Time) (bool, error) {
	q := idempotencydb.New(p.pool)
	deleted, err := q.ReleaseKey(ctx, idempotencydb.ReleaseKeyParams{
		MerchantID: uuid(held.Merchant), Key: held.Key, LockToken: held.Token[:],
	})
	if err != nil {
		return false, fmt.Errorf("idempotency: release: %w", err)
	}
	if deleted == 1 {
		return true, nil
	}
	ended, err := q.EndLease(ctx, idempotencydb.EndLeaseParams{
		MerchantID: uuid(held.Merchant), Key: held.Key, LockToken: held.Token[:], Now: timestamptz(now),
	})
	if err != nil {
		return false, fmt.Errorf("idempotency: end lease: %w", err)
	}
	if ended == 0 {
		return false, app.ErrLeaseLost
	}
	return false, nil
}

func (p *Postgres) DeleteExpired(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	n, err := idempotencydb.New(p.pool).DeleteExpiredKeys(ctx, idempotencydb.DeleteExpiredKeysParams{
		Cutoff:  timestamptz(cutoff),
		MaxRows: int64(limit),
	})
	if err != nil {
		return 0, fmt.Errorf("idempotency: delete expired: %w", err)
	}
	return n, nil
}

// SetRecoveryPoint records how far the request got. It runs on the caller's
// transaction, so the recovery point commits or rolls back with the work it describes;
// if the lease was lost it returns ErrLeaseLost and the caller's work must roll back.
func SetRecoveryPoint(ctx context.Context, db idempotencydb.DBTX, held *domain.Held, point domain.RecoveryPoint, resource [16]byte) error {
	n, err := idempotencydb.New(db).SetRecoveryPoint(ctx, idempotencydb.SetRecoveryPointParams{
		MerchantID:    uuid(held.Merchant),
		Key:           held.Key,
		LockToken:     held.Token[:],
		RecoveryPoint: string(point),
		ResourceID:    uuid(resource),
	})
	if err != nil {
		return fmt.Errorf("idempotency: set recovery point: %w", err)
	}
	if n == 0 {
		return app.ErrLeaseLost
	}
	return nil
}
