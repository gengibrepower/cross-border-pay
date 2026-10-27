package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/events/adapters/eventsdb"
	"github.com/gengibrepower/cross-border-pay/internal/events/app"
	"github.com/gengibrepower/cross-border-pay/internal/events/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
)

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

func (p *Postgres) Insert(ctx context.Context, in *domain.Incoming, now time.Time) (inserted bool, stored []byte, err error) {
	q := eventsdb.New(p.pool)
	_, err = q.InsertEvent(ctx, eventsdb.InsertEventParams{
		Provider:   in.Provider,
		ExternalID: in.ExternalID,
		Type:       in.Type,
		Payload:    in.Payload,
		Signature:  in.Signature,
		ReceivedAt: timestamptz(now),
	})
	if err == nil {
		return true, nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, nil, fmt.Errorf("events: insert: %w", err)
	}
	stored, err = q.StoredPayload(ctx, eventsdb.StoredPayloadParams{Provider: in.Provider, ExternalID: in.ExternalID})
	if err != nil {
		return false, nil, fmt.Errorf("events: stored payload: %w", err)
	}
	return false, stored, nil
}

func (p *Postgres) ProcessNext(ctx context.Context, now time.Time, handle func(context.Context, pgx.Tx, *domain.Event) (app.Result, error)) (app.Done, bool, error) {
	var d app.Done
	found := false
	err := postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		q := eventsdb.New(tx)
		row, err := q.ClaimDueEvent(ctx, timestamptz(now))
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim: %w", err)
		}
		found = true
		d = app.Done{Event: restore(&row)}
		d.Result, err = handle(ctx, tx, &d.Event)
		if err != nil {
			return fmt.Errorf("handle %s event %s: %w", d.Event.Provider, d.Event.ExternalID, err)
		}
		status, retryAt, err := d.Event.Next(d.Result.Outcome, now)
		if err != nil {
			return fmt.Errorf("event %s: %w", d.Event.ExternalID, err)
		}
		d.Status = status
		return record(ctx, q, &d, status, retryAt, now)
	})
	if err != nil {
		return d, found, fmt.Errorf("events: %w", err)
	}
	return d, found, nil
}

func record(ctx context.Context, q *eventsdb.Queries, d *app.Done, status domain.Status, retryAt, now time.Time) error {
	id := pgtype.UUID{Bytes: d.Event.ID, Valid: true}
	if status == domain.StatusPending {
		if _, err := q.RetryEvent(ctx, eventsdb.RetryEventParams{ID: id, NextAttemptAt: timestamptz(retryAt), LastError: text(d.Result.Reason)}); err != nil {
			return fmt.Errorf("retry: %w", err)
		}
		return nil
	}
	_, err := q.FinishEvent(ctx, eventsdb.FinishEventParams{
		ID:          id,
		Status:      string(status),
		SubjectID:   text(d.Result.Subject),
		LastError:   text(d.Result.Reason),
		ProcessedAt: timestamptz(now),
	})
	if err != nil {
		return fmt.Errorf("finish: %w", err)
	}
	return nil
}

// Retry runs after the handler's transaction rolled back, so the event's row is no
// longer locked; the queries only touch it while it is still pending.
func (p *Postgres) Retry(ctx context.Context, e *domain.Event, reason string, now time.Time) (domain.Status, error) {
	status, retryAt, err := e.Next(domain.Retry, now)
	if err != nil {
		return "", fmt.Errorf("events: %w", err)
	}
	d := app.Done{Event: *e, Result: app.Result{Outcome: domain.Retry, Reason: reason}}
	err = postgres.WithTx(ctx, p.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		return record(ctx, eventsdb.New(tx), &d, status, retryAt, now)
	})
	if err != nil {
		return "", fmt.Errorf("events: retry %s: %w", e.ExternalID, err)
	}
	return status, nil
}

func restore(row *eventsdb.InboxEvent) domain.Event {
	return domain.Event{
		ID:         row.ID.Bytes,
		Provider:   row.Provider,
		ExternalID: row.ExternalID,
		Type:       row.Type,
		Payload:    row.Payload,
		Attempts:   int(row.Attempts),
		ReceivedAt: row.ReceivedAt.Time.UTC(),
	}
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}
