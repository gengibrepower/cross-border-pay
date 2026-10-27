package adapters

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gengibrepower/cross-border-pay/internal/events/adapters/eventsdb"
	"github.com/gengibrepower/cross-border-pay/internal/events/domain"
)

// Outbox keeps no state: Append runs in the caller's transaction, so the event commits
// with the fact it reports or not at all.
type Outbox struct{}

func (Outbox) Append(ctx context.Context, tx pgx.Tx, e *domain.Outgoing) error {
	err := eventsdb.New(tx).AppendOutboxEvent(ctx, eventsdb.AppendOutboxEventParams{
		ID:         pgtype.UUID{Bytes: e.ID, Valid: true},
		MerchantID: pgtype.UUID{Bytes: e.Merchant, Valid: true},
		Type:       e.Type,
		SubjectID:  e.Subject,
		Payload:    e.Payload,
		CreatedAt:  timestamptz(e.CreatedAt),
	})
	if err != nil {
		return fmt.Errorf("events: append %s for %s: %w", e.Type, e.Subject, err)
	}
	return nil
}
