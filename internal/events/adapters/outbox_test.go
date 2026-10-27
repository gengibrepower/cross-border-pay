package adapters_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gengibrepower/cross-border-pay/internal/events/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/events/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

// The payload is kept byte for byte, whitespace and key order included: it is what
// every delivery signs.
func TestOutboxKeepsThePayloadAsSent(t *testing.T) {
	pool := pgtest.NewPool(t)
	var merchant [16]byte
	if err := pool.QueryRow(t.Context(), "INSERT INTO merchants (name) VALUES ('m') RETURNING id").Scan(&merchant); err != nil {
		t.Fatal(err)
	}
	e := domain.Outgoing{
		ID: [16]byte{1}, Merchant: merchant, Type: "payment.paid", Subject: "pay_1",
		Payload: []byte(`{"z": 1,  "a":2}`), CreatedAt: time.Date(2026, 11, 3, 14, 21, 7, 123456000, time.UTC),
	}
	err := postgres.WithTx(t.Context(), pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		return adapters.Outbox{}.Append(ctx, tx, &e)
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload []byte
	var typ, subject string
	var at time.Time
	err = pool.QueryRow(t.Context(), "SELECT payload, type, subject_id, created_at FROM outbox_events WHERE id = $1", e.ID).
		Scan(&payload, &typ, &subject, &at)
	if err != nil || !bytes.Equal(payload, e.Payload) || typ != e.Type || subject != e.Subject || !at.Equal(e.CreatedAt) {
		t.Errorf("stored %q %s %s %s (%v), want %+v", payload, typ, subject, at, err, e)
	}
}

func TestOutboxRefusesAnUnknownMerchant(t *testing.T) {
	pool := pgtest.NewPool(t)
	e := domain.Outgoing{ID: [16]byte{1}, Merchant: [16]byte{9}, Type: "payment.paid", Subject: "pay_1", Payload: []byte("{}"), CreatedAt: time.Now()}
	err := postgres.WithTx(t.Context(), pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		return adapters.Outbox{}.Append(ctx, tx, &e)
	})
	if err == nil {
		t.Error("stored an event for a merchant that does not exist")
	}
}
