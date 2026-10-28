package adapters_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

// A cancel and an on-time confirmation of the same payment race. Whichever takes the
// payment's lock first wins and the other sees its status: paid with its posting, or
// canceled with the money held for review. Never both, never neither.
func TestCancelAndConfirmationRace(t *testing.T) {
	pool := pgtest.NewPool(t)
	store := adapters.NewPostgres(pool, sinks())
	const rounds = 20
	outcomes := map[string]int{}
	for range rounds {
		id := newPayment(t, pool)
		a := create(t, store, id)
		charge := "ch_" + a.ID().String()
		apply(t, store, a.ID(), domain.Send)
		if _, _, err := store.ApplyAttempt(t.Context(), a.ID(), &domain.AttemptUpdate{Event: domain.Accept, ExternalID: charge}, t0); err != nil {
			t.Fatal(err)
		}
		var merchant domain.MerchantID
		if err := pool.QueryRow(t.Context(), "SELECT merchant_id FROM payments WHERE id = $1", id).Scan(&merchant); err != nil {
			t.Fatal(err)
		}

		var cancelErr, applyErr error
		var wg sync.WaitGroup
		wg.Go(func() { cancelErr = store.Cancel(t.Context(), merchant, id, t0) })
		wg.Go(func() {
			_, _, applyErr = store.ApplyAttempt(t.Context(), a.ID(), &domain.AttemptUpdate{Event: domain.Succeed, ExternalID: charge, PaidAt: t0, Amount: quoted(t)}, t0)
		})
		wg.Wait()
		if applyErr != nil {
			t.Fatalf("confirmation: %v", applyErr)
		}

		var status string
		var posted, held int
		err := pool.QueryRow(t.Context(), `SELECT status,
			(SELECT count(*) FROM ledger_transactions WHERE reference = $2),
			(SELECT count(*) FROM review_cases WHERE payment_id = $1)
			FROM payments WHERE id = $1`, id, domain.ConfirmationReference(id)).Scan(&status, &posted, &held)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case status == "paid" && posted == 1 && held == 0 && errors.Is(cancelErr, domain.ErrInvalidTransition):
		case status == "canceled" && posted == 0 && held == 1 && cancelErr == nil:
		default:
			t.Fatalf("status %s, %d postings, %d review cases, cancel error %v", status, posted, held, cancelErr)
		}
		outcomes[status]++
	}
	t.Logf("outcomes over %d rounds: %v", rounds, outcomes)
}
