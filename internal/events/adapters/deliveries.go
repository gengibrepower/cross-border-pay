package adapters

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

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
			Attempt: int(r.Attempts),
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
