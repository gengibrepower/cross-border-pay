package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	ledgeradapters "github.com/gengibrepower/cross-border-pay/internal/ledger/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters/paymentsdb"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// errUnpostable is money the provider confirmed that the ledger cannot hold: without its
// charge, without an amount, or in a currency the payment was not quoted in.
var errUnpostable = errors.New("the provider confirmed money the ledger cannot hold")

// diverge posts money the provider confirmed that confirms nothing to the divergence
// account and opens its review case (section 4.4). The case's unique charge is what
// makes a charge reported twice, by its event and by the expiry job, post once: only
// the call that opens it posts. The payment is left as it is.
func diverge(ctx context.Context, tx pgx.Tx, reviews Reviews, pay *paymentsdb.LockPaymentOfAttemptRow, attempt *domain.PaymentAttempt, kind domain.DivergenceKind, u *domain.AttemptUpdate, cause error, now time.Time) error {
	charge := u.ExternalID
	if charge == "" {
		charge = attempt.ExternalID()
	}
	if charge == "" || u.Amount.Amount() <= 0 || u.Amount.Currency().Code() != pay.QuoteCurrency {
		return fmt.Errorf("divergence of attempt %s: %w: %q, %s", attempt.ID(), errUnpostable, charge, u.Amount)
	}
	reference := domain.DivergenceReference(attempt.Provider(), charge)
	opened, err := reviews.Open(ctx, tx, &domain.ReviewCase{
		Kind:            kind,
		Provider:        attempt.Provider(),
		ExternalID:      charge,
		Payment:         pay.PaymentID.Bytes,
		Attempt:         attempt.ID(),
		Amount:          u.Amount,
		LedgerReference: reference,
		Details:         cause.Error(),
		CreatedAt:       now,
	})
	if err != nil {
		return fmt.Errorf("review case: %w", err)
	}
	if !opened {
		return nil
	}

	cur := u.Amount.Currency().Code()
	receivables, held := "provider_receivables:"+cur, "divergence:"+cur
	l := ledgeradapters.Ledger{}
	accounts, err := l.AccountsByCode(ctx, tx, receivables, held)
	if err != nil {
		return fmt.Errorf("chart of accounts: %w", err)
	}
	t, err := domain.DivergencePosting(reference, accounts[receivables].ID(), accounts[held].ID(), u.Amount)
	if err != nil {
		return fmt.Errorf("divergence: %w", err)
	}
	if _, err := l.Post(ctx, tx, t); err != nil {
		return fmt.Errorf("post: %w", err)
	}
	return nil
}
