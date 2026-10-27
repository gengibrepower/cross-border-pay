package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gengibrepower/cross-border-pay/internal/reconciliation/adapters/reconciliationdb"
	"github.com/gengibrepower/cross-border-pay/internal/reconciliation/domain"
)

// Reviews keeps no state: Open runs in the caller's transaction, so the case commits
// with the posting it names or not at all.
type Reviews struct{}

// Open stores the case unless its charge already has one; opened says which. A charge
// seen again is not an error, so the caller can skip what it already did.
func (Reviews) Open(ctx context.Context, tx pgx.Tx, c *domain.Case) (opened bool, err error) {
	_, err = reconciliationdb.New(tx).OpenCase(ctx, reconciliationdb.OpenCaseParams{
		Kind:            string(c.Kind),
		Provider:        c.Provider,
		ExternalID:      c.ExternalID,
		PaymentID:       optionalUUID(c.Payment),
		AttemptID:       optionalUUID(c.Attempt),
		Amount:          c.Amount.Amount(),
		Currency:        c.Amount.Currency().Code(),
		LedgerReference: c.LedgerReference,
		Details:         c.Details,
		CreatedAt:       pgtype.Timestamptz{Time: c.CreatedAt, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reconciliation: open %s case for %s %s: %w", c.Kind, c.Provider, c.ExternalID, err)
	}
	return true, nil
}

func optionalUUID(id *[16]byte) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}
