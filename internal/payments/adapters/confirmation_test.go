package adapters_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
)

// gameStoreBalances is section 3.3's confirmation, by account code; merchant stands
// for the merchant's own payable.
var gameStoreBalances = map[string]int64{
	"provider_receivables:BRL": 10150,
	"fx:BRL":                   -10150,
	"fx:USD":                   -2030,
	"merchant":                 1940,
	"fee_revenue:USD":          60,
	"spread_revenue:USD":       30,
}

func (e *eventEnv) balances() map[string]int64 {
	e.t.Helper()
	rows, err := e.pool.Query(e.t.Context(), `SELECT a.code, sum(en.amount)::bigint
		FROM ledger_entries AS en JOIN ledger_accounts AS a ON a.id = en.account_id GROUP BY a.code`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var code string
		var sum int64
		if err := rows.Scan(&code, &sum); err != nil {
			e.t.Fatal(err)
		}
		if strings.HasPrefix(code, "merchant_payable:") {
			code = "merchant"
		}
		got[code] += sum
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return got
}

func (e *eventEnv) paymentStatus(id domain.AttemptID) string {
	e.t.Helper()
	var status string
	err := e.pool.QueryRow(e.t.Context(),
		"SELECT p.status FROM payments p JOIN payment_attempts a ON a.payment_id = p.id WHERE a.id = $1", id).Scan(&status)
	if err != nil {
		e.t.Fatal(err)
	}
	return status
}

func (e *eventEnv) confirmations(id domain.AttemptID) int {
	e.t.Helper()
	var n int
	err := e.pool.QueryRow(e.t.Context(), "SELECT count(*) FROM ledger_transactions WHERE reference = $1",
		domain.ConfirmationReference(e.paymentID(id))).Scan(&n)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *eventEnv) paymentID(id domain.AttemptID) domain.PaymentID {
	a := e.status(id)
	return a.Payment()
}

// resolve is the resolver's path: it found the charge chg_1 paid at the provider.
func (e *eventEnv) resolve(ctx context.Context, id domain.AttemptID) error {
	return e.resolveCharge(ctx, id, "chg_1")
}

func (e *eventEnv) resolveCharge(ctx context.Context, id domain.AttemptID, charge string) error {
	_, _, err := e.store.ApplyAttempt(ctx, id, domain.AttemptUpdate{Event: domain.Succeed, ExternalID: charge, BRCode: paidCode}, t0.Add(2*time.Minute))
	return err
}

func TestBothPathsConfirmThePayment(t *testing.T) {
	for name, succeed := range map[string]func(*eventEnv, domain.AttemptID, money.Money) error{
		"event": func(e *eventEnv, id domain.AttemptID, amount money.Money) error {
			event := paid(id.String(), amount)
			_, err := e.handle(t.Context(), &event)
			return err
		},
		"resolver": func(e *eventEnv, id domain.AttemptID, _ money.Money) error { return e.resolve(t.Context(), id) },
	} {
		e := newEventEnv(t)
		id, amount := e.attempt(domain.Send, domain.Accept)
		if err := succeed(e, id, amount); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s := e.paymentStatus(id); s != string(domain.Paid) {
			t.Errorf("%s: payment %s, want paid", name, s)
		}
		if got := e.balances(); !maps.Equal(got, gameStoreBalances) {
			t.Errorf("%s: balances %v, want %v", name, got, gameStoreBalances)
		}
		if events, version := e.outbox(id), e.version(id); len(events) != 1 || version != 2 {
			t.Errorf("%s: %d outbox events and version %d, want 1 and 2", name, len(events), version)
		}
	}
}

// Cited test 1 (section 9): a thousand confirmations of one charge at once, by event
// and by resolver, post exactly one transaction.
func TestThousandConcurrentConfirmationsPostOnce(t *testing.T) {
	e := newEventEnv(t)
	id, amount := e.attempt(domain.Send, domain.Accept)
	event := paid(id.String(), amount)
	const confirmations = 1000
	var wg sync.WaitGroup
	for i := range confirmations {
		wg.Go(func() {
			var err error
			if i%2 == 0 {
				_, err = e.handle(t.Context(), &event)
			} else {
				err = e.resolve(t.Context(), id)
			}
			if err != nil {
				t.Errorf("confirmation %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	if n := e.confirmations(id); n != 1 {
		t.Errorf("%d confirmation transactions, want 1", n)
	}
	if n := len(e.outbox(id)); n != 1 {
		t.Errorf("%d payment.paid events, want 1", n)
	}
	if s := e.paymentStatus(id); s != string(domain.Paid) {
		t.Errorf("payment %s, want paid", s)
	}
	if got := e.balances(); !maps.Equal(got, gameStoreBalances) {
		t.Errorf("balances %v, want %v", got, gameStoreBalances)
	}
}

// The attempt, the payment and the ledger commit together: a ledger that cannot post
// leaves the attempt and the payment as they were.
func TestLedgerFailureUndoesTheConfirmation(t *testing.T) {
	e := newEventEnv(t)
	id, _ := e.attempt(domain.Send, domain.Accept)
	if _, err := e.pool.Exec(t.Context(), "DELETE FROM ledger_accounts WHERE code = 'fee_revenue:USD'"); err != nil {
		t.Fatal(err)
	}
	if err := e.resolve(t.Context(), id); err == nil {
		t.Fatal("confirmed without a fee account")
	}
	a := e.status(id)
	if a.Status() != domain.AttemptAccepted || e.paymentStatus(id) != string(domain.Pending) || len(e.balances()) != 0 {
		t.Errorf("attempt %s, payment %s, balances %v; want accepted, pending and nothing posted",
			a.Status(), e.paymentStatus(id), e.balances())
	}
}

// Every path takes the payment's lock before the attempt's, so two paths never hold
// one each while waiting for the other. While a path waits for the payment, the attempt
// must still be free.
func TestPathsLockThePaymentBeforeTheAttempt(t *testing.T) {
	for name, succeed := range map[string]func(*eventEnv, context.Context, domain.AttemptID, money.Money) error{
		"event": func(e *eventEnv, ctx context.Context, id domain.AttemptID, amount money.Money) error {
			event := paid(id.String(), amount)
			_, err := e.handle(ctx, &event)
			return err
		},
		"resolver": func(e *eventEnv, ctx context.Context, id domain.AttemptID, _ money.Money) error {
			return e.resolve(ctx, id)
		},
	} {
		e := newEventEnv(t)
		id, amount := e.attempt(domain.Send, domain.Accept)
		holder, err := e.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := holder.Exec(t.Context(), "SELECT 1 FROM payments FOR NO KEY UPDATE"); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() { done <- succeed(e, t.Context(), id, amount) }()
		e.waitForALockWait()

		probe, err := e.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = probe.Exec(t.Context(), "SELECT 1 FROM payment_attempts WHERE id = $1 FOR UPDATE NOWAIT", id)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			t.Errorf("%s: holds the attempt while waiting for the payment", name)
		} else if err != nil {
			t.Fatal(err)
		}
		_ = probe.Rollback(t.Context())
		_ = holder.Rollback(t.Context())

		if err := <-done; err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if s := e.paymentStatus(id); s != string(domain.Paid) {
			t.Errorf("%s: payment %s, want paid", name, s)
		}
	}
}

func (e *eventEnv) waitForALockWait() {
	e.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var waiting int
		err := e.pool.QueryRow(e.t.Context(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		if err != nil {
			e.t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
	}
	e.t.Fatal("no transaction waited for the payment's lock within 5 s")
}

// A payment already paid, by whatever path, is not posted again when its attempt
// succeeds (invariant 7).
func TestAlreadyPaidIsNotPostedAgain(t *testing.T) {
	e := newEventEnv(t)
	id, _ := e.attempt(domain.Send, domain.Accept)
	if _, err := e.pool.Exec(t.Context(), "UPDATE payments SET status = 'paid'"); err != nil {
		t.Fatal(err)
	}
	if err := e.resolve(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if n := e.confirmations(id); n != 0 {
		t.Errorf("%d confirmation transactions for a payment already paid, want 0", n)
	}
	if n := len(e.outbox(id)); n != 0 {
		t.Errorf("%d events for a payment already paid, want 0", n)
	}
}

// Each merchant is owed on its own account.
func TestEachMerchantHasItsOwnPayable(t *testing.T) {
	e := newEventEnv(t)
	for _, charge := range []string{"chg_1", "chg_2"} {
		id, _ := e.attempt(domain.Send, domain.Accept)
		if err := e.resolveCharge(t.Context(), id, charge); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := e.pool.Query(t.Context(), `SELECT sum(en.amount)::bigint FROM ledger_entries AS en
		JOIN ledger_accounts AS a ON a.id = en.account_id WHERE a.code LIKE 'merchant_payable:%' GROUP BY a.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var owed []int64
	for rows.Next() {
		var sum int64
		if err := rows.Scan(&sum); err != nil {
			t.Fatal(err)
		}
		owed = append(owed, sum)
	}
	if len(owed) != 2 || owed[0] != 1940 || owed[1] != 1940 {
		t.Errorf("payables %v, want two of 1940", owed)
	}
}

func (e *eventEnv) outbox(id domain.AttemptID) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.t.Context(), "SELECT type FROM outbox_events WHERE subject_id = $1", e.paymentID(id).String())
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			e.t.Fatal(err)
		}
		types = append(types, typ)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return types
}

func (e *eventEnv) version(id domain.AttemptID) int {
	e.t.Helper()
	var v int
	if err := e.pool.QueryRow(e.t.Context(), "SELECT version FROM payments WHERE id = $1", e.paymentID(id)).Scan(&v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

type failingOutbox struct{}

func (failingOutbox) Append(context.Context, pgx.Tx, *domain.MerchantEvent) error {
	return errors.New("outbox down")
}

// Section 4.5: the event commits with the fact or not at all. An outbox that cannot
// take it undoes the attempt, the payment and the ledger.
func TestOutboxFailureUndoesTheConfirmation(t *testing.T) {
	e := newEventEnv(t)
	id, _ := e.attempt(domain.Send, domain.Accept)
	broken := adapters.NewPostgres(e.pool, failingOutbox{})
	u := domain.AttemptUpdate{Event: domain.Succeed, ExternalID: "chg_1", BRCode: paidCode}
	if _, _, err := broken.ApplyAttempt(t.Context(), id, u, t0.Add(2*time.Minute)); err == nil {
		t.Fatal("confirmed without an outbox")
	}
	a := e.status(id)
	if a.Status() != domain.AttemptAccepted || e.paymentStatus(id) != string(domain.Pending) || e.version(id) != 1 || len(e.balances()) != 0 {
		t.Errorf("attempt %s, payment %s v%d, balances %v; want accepted, pending v1 and nothing posted",
			a.Status(), e.paymentStatus(id), e.version(id), e.balances())
	}
}
