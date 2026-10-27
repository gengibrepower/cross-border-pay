package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/events/adapters/eventsdb"
	"github.com/gengibrepower/cross-border-pay/internal/events/domain"
)

// maxErrorLength matches the column that keeps the last delivery error.
const maxErrorLength = 500

// Deliveries is the outbox seen by the worker that sends it. Each call is its own
// short transaction: nothing here stays open while an event is posted.
type Deliveries struct {
	pool *pgxpool.Pool
}

func NewDeliveries(pool *pgxpool.Pool) *Deliveries {
	return &Deliveries{pool: pool}
}

// Claim takes up to perMerchant due events of each merchant, those with the oldest
// first, at most maxEvents in all, and leases them until leaseUntil.
func (d *Deliveries) Claim(ctx context.Context, now, leaseUntil time.Time, perMerchant, maxEvents int) ([]domain.Delivery, error) {
	rows, err := eventsdb.New(d.pool).ClaimDueDeliveries(ctx, eventsdb.ClaimDueDeliveriesParams{
		Now:          timestamptz(now),
		LeaseUntil:   timestamptz(leaseUntil),
		MaxMerchants: int32(maxEvents),   //nolint:gosec // G115: a batch size, set in code far below 2^31.
		PerMerchant:  int32(perMerchant), //nolint:gosec // G115: as above.
		MaxEvents:    int32(maxEvents),   //nolint:gosec // G115: as above.
	})
	if err != nil {
		return nil, fmt.Errorf("events: claim deliveries: %w", err)
	}
	out := make([]domain.Delivery, len(rows))
	for i := range rows {
		r := &rows[i]
		out[i] = domain.Delivery{
			Event: domain.Outgoing{
				ID: r.ID.Bytes, Merchant: r.MerchantID.Bytes, Type: r.Type, Subject: r.SubjectID,
				Payload: r.Payload, CreatedAt: r.CreatedAt.Time.UTC(),
			},
			Attempt:            int(r.Attempts),
			WindowStart:        r.WindowStartedAt.Time.UTC(),
			WindowFirstAttempt: int(r.WindowFirstAttempt),
		}
	}
	return out, nil
}

// Delivered records a 2xx. It reports false when the event was no longer pending.
func (d *Deliveries) Delivered(ctx context.Context, dl *domain.Delivery, statusCode int, now time.Time) (bool, error) {
	n, err := eventsdb.New(d.pool).MarkDelivered(ctx, eventsdb.MarkDeliveredParams{
		ID: pgtype.UUID{Bytes: dl.Event.ID, Valid: true}, StatusCode: statusInt4(statusCode), Now: timestamptz(now),
	})
	if err != nil {
		return false, fmt.Errorf("events: mark %x delivered: %w", dl.Event.ID, err)
	}
	return n == 1, nil
}

// Reschedule records a failed attempt and when to try again. statusCode is 0 when no
// answer came. It reports false when the claim was no longer current.
func (d *Deliveries) Reschedule(ctx context.Context, dl *domain.Delivery, statusCode int, reason string, at time.Time) (bool, error) {
	n, err := eventsdb.New(d.pool).RescheduleDelivery(ctx, eventsdb.RescheduleDeliveryParams{
		ID: pgtype.UUID{Bytes: dl.Event.ID, Valid: true}, Attempts: int32(dl.Attempt), //nolint:gosec // G115: read from an integer column.
		NextAttemptAt: timestamptz(at), StatusCode: statusInt4(statusCode), LastError: text(truncate(reason)),
	})
	if err != nil {
		return false, fmt.Errorf("events: reschedule delivery %x: %w", dl.Event.ID, err)
	}
	return n == 1, nil
}

// Fail gives the event up after a failed attempt. It reports false when the claim was
// no longer current.
func (d *Deliveries) Fail(ctx context.Context, dl *domain.Delivery, statusCode int, reason string, now time.Time) (bool, error) {
	n, err := eventsdb.New(d.pool).FailDelivery(ctx, eventsdb.FailDeliveryParams{
		ID: pgtype.UUID{Bytes: dl.Event.ID, Valid: true}, Attempts: int32(dl.Attempt), //nolint:gosec // G115: read from an integer column.
		Now: timestamptz(now), StatusCode: statusInt4(statusCode), LastError: text(truncate(reason)),
	})
	if err != nil {
		return false, fmt.Errorf("events: fail delivery %x: %w", dl.Event.ID, err)
	}
	return n == 1, nil
}

func (d *Deliveries) DeleteDelivered(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	n, err := eventsdb.New(d.pool).DeleteDeliveredEvents(ctx, eventsdb.DeleteDeliveredEventsParams{
		Cutoff: timestamptz(cutoff), MaxRows: int64(limit),
	})
	if err != nil {
		return n, fmt.Errorf("events: delete delivered: %w", err)
	}
	return n, nil
}

// List returns one page of the merchant's events, newest first.
func (d *Deliveries) List(ctx context.Context, q *domain.ListQuery) ([]domain.Listed, error) {
	rows, err := eventsdb.New(d.pool).ListOutboxEvents(ctx, eventsdb.ListOutboxEventsParams{
		MerchantID:     pgtype.UUID{Bytes: q.Merchant, Valid: true},
		Before:         pgtype.UUID{Bytes: q.Before, Valid: q.Before != [16]byte{}},
		DeliveryStatus: text(string(q.Status)),
		MaxRows:        int64(q.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("events: list outbox: %w", err)
	}
	out := make([]domain.Listed, len(rows))
	for i := range rows {
		r := &rows[i]
		out[i] = listed(&outboxRow{
			r.ID, r.MerchantID, r.Type, r.SubjectID, r.Payload, r.CreatedAt, r.DeliveryStatus, r.Attempts,
			r.NextAttemptAt, r.LastStatusCode, r.LastError, r.FinishedAt,
		})
	}
	return out, nil
}

// Redeliver makes the merchant's event due at now, from any status, in a new retry
// window. It returns domain.ErrOutgoingNotFound for an ID the merchant has no event at.
func (d *Deliveries) Redeliver(ctx context.Context, merchant, id [16]byte, now time.Time) (domain.Listed, error) {
	r, err := eventsdb.New(d.pool).RedeliverOutboxEvent(ctx, eventsdb.RedeliverOutboxEventParams{
		ID: pgtype.UUID{Bytes: id, Valid: true}, MerchantID: pgtype.UUID{Bytes: merchant, Valid: true}, Now: timestamptz(now),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Listed{}, domain.ErrOutgoingNotFound
	}
	if err != nil {
		return domain.Listed{}, fmt.Errorf("events: redeliver %x: %w", id, err)
	}
	return listed(&outboxRow{
		r.ID, r.MerchantID, r.Type, r.SubjectID, r.Payload, r.CreatedAt, r.DeliveryStatus, r.Attempts,
		r.NextAttemptAt, r.LastStatusCode, r.LastError, r.FinishedAt,
	}), nil
}

// outboxRow is the shape both ListOutboxEventsRow and RedeliverOutboxEventRow have;
// sqlc gives each query its own type.
type outboxRow struct {
	id, merchant   pgtype.UUID
	typ, subject   string
	payload        []byte
	createdAt      pgtype.Timestamptz
	status         string
	attempts       int32
	nextAttemptAt  pgtype.Timestamptz
	lastStatusCode pgtype.Int4
	lastError      pgtype.Text
	finishedAt     pgtype.Timestamptz
}

func listed(r *outboxRow) domain.Listed {
	return domain.Listed{
		Event: domain.Outgoing{
			ID: r.id.Bytes, Merchant: r.merchant.Bytes, Type: r.typ, Subject: r.subject, Payload: r.payload,
			CreatedAt: r.createdAt.Time.UTC(),
		},
		Status:         domain.DeliveryStatus(r.status),
		Attempts:       int(r.attempts),
		NextAttemptAt:  utc(r.nextAttemptAt),
		LastStatusCode: int(r.lastStatusCode.Int32),
		LastError:      r.lastError.String,
		FinishedAt:     utc(r.finishedAt),
	}
}

// utc keeps times in UTC (section 4.9): pgx reads timestamptz in the local zone. A NULL
// stays the zero time.
func utc(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

func statusInt4(code int) pgtype.Int4 {
	return pgtype.Int4{Int32: int32(code), Valid: code != 0} //nolint:gosec // G115: an HTTP status code.
}

// truncate cuts at a rune boundary, so the column never holds broken UTF-8.
func truncate(s string) string {
	if len(s) <= maxErrorLength {
		return s
	}
	cut := maxErrorLength
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
