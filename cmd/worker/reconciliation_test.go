package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
	reconciliationadapters "github.com/gengibrepower/cross-border-pay/internal/reconciliation/adapters"
	reconciliationapp "github.com/gengibrepower/cross-border-pay/internal/reconciliation/app"
)

// fixedStatement is a provider whose statement of t0's day is movements.
type fixedStatement []simkit.Movement

func (f fixedStatement) Statement(_ context.Context, date calendar.Date) (simkit.Statement, error) {
	if date.String() != "2026-11-01" {
		return simkit.Statement{Date: date}, nil
	}
	return simkit.Statement{Date: date, Movements: f}, nil
}

func (e *inboxEnv) reconciler(source statementSource) *reconciliationapp.Reconciler {
	e.t.Helper()
	cal, err := calendar.Brazil()
	if err != nil {
		e.t.Fatal(err)
	}
	return reconciliationapp.NewReconciler(statements{"pixsim": source}, books{e.payments},
		reconciliationadapters.NewCases(e.pool), &cal, e.clock)
}

func (e *inboxEnv) reconcile(movements ...simkit.Movement) reconciliationapp.Report {
	e.t.Helper()
	rep, err := e.reconciler(fixedStatement(movements)).Reconcile(e.t.Context(), "pixsim", calendar.On(t0, time.UTC))
	if err != nil {
		e.t.Fatalf("Reconcile() error = %v", err)
	}
	return rep
}

func (e *inboxEnv) cases() map[string]bool {
	e.t.Helper()
	rows, err := e.pool.Query(e.t.Context(), "SELECT kind, ledger_reference IS NOT NULL FROM review_cases")
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var kind string
		var posted bool
		if err := rows.Scan(&kind, &posted); err != nil {
			e.t.Fatal(err)
		}
		out[kind] = posted
	}
	return out
}

func (e *inboxEnv) balance(code string) int64 {
	e.t.Helper()
	var b int64
	err := e.pool.QueryRow(e.t.Context(), `SELECT coalesce(sum(en.amount), 0) FROM ledger_entries AS en
		JOIN ledger_accounts AS l ON l.id = en.account_id WHERE l.code = $1`, code).Scan(&b)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func received(key, external string, amount money.Money) simkit.Movement {
	return simkit.Movement{Kind: simkit.Received, ID: external, IdempotencyKey: key, Amount: amount, At: t0}
}

// The payer paid but the event never came: the statement confirms the payment, as the
// event would, and the next run finds both sides agreeing.
func TestReconciliationConfirmsAPaymentWhoseEventWasLost(t *testing.T) {
	e := newInboxEnv(t)
	id, amount := e.attempt()
	e.apply(id, &payments.AttemptUpdate{Event: payments.Send})
	e.apply(id, &payments.AttemptUpdate{Event: payments.Accept, ExternalID: "ch_1", BRCode: "br"})

	line := received(id.String(), "ch_1", amount)
	// Another day's divergence is that day's run to handle.
	yesterday := received("someone-else", "ch_0", amount)
	yesterday.At = t0.AddDate(0, 0, -1)
	if rep := e.reconcile(line, yesterday); rep != (reconciliationapp.Report{Divergences: 1, Settled: 1}) {
		t.Fatalf("report = %+v, want one settled", rep)
	}
	if a, err := e.payments.GetAttempt(t.Context(), id); err != nil || a.Status() != payments.AttemptSucceeded {
		t.Fatalf("attempt %s (%v), want succeeded", a.Status(), err)
	}
	if rep := e.reconcile(line); rep != (reconciliationapp.Report{}) {
		t.Errorf("second run = %+v, want nothing", rep)
	}
	if got := e.cases(); len(got) != 0 {
		t.Errorf("cases = %v, want none", got)
	}
}

// Money for a key the gateway never gave is held, as section 4.4 holds money that
// confirms nothing, and the held money is the gateway's side next time.
func TestReconciliationHoldsMoneyForNoAttempt(t *testing.T) {
	e := newInboxEnv(t)
	// A key no one gave, and one shaped as an attempt's that names none.
	lines := []simkit.Movement{
		received("someone-else", "ch_1", money.New(500, money.BRL)),
		received(payments.AttemptID{9}.String(), "ch_2", money.New(500, money.BRL)),
	}
	if rep := e.reconcile(lines...); rep != (reconciliationapp.Report{Divergences: 2, Settled: 2}) {
		t.Fatalf("report = %+v, want both settled", rep)
	}
	if rep := e.reconcile(lines...); rep != (reconciliationapp.Report{}) {
		t.Errorf("second run = %+v, want nothing", rep)
	}
	var n int
	if err := e.pool.QueryRow(t.Context(), "SELECT count(*) FROM review_cases WHERE kind = 'missing_in_system' AND ledger_reference IS NOT NULL AND attempt_id IS NULL").Scan(&n); err != nil || n != 2 {
		t.Errorf("%d missing_in_system cases with a posting (%v), want 2", n, err)
	}
	if got := e.balance("divergence:BRL"); got != -1000 {
		t.Errorf("divergence:BRL = %d, want both 500 held", got)
	}
}

// What only the gateway has, or has for another amount, is for a person: a case with
// no posting, opened once.
func TestReconciliationReportsWhatOnlyAPersonCanPlace(t *testing.T) {
	e := newInboxEnv(t)
	id, amount := e.attempt()
	e.apply(id, &payments.AttemptUpdate{Event: payments.Send})
	e.apply(id, &payments.AttemptUpdate{Event: payments.Accept, ExternalID: "ch_1", BRCode: "br"})
	e.apply(id, &payments.AttemptUpdate{Event: payments.Succeed, ExternalID: "ch_1", Amount: amount, PaidAt: t0})
	other, _ := e.attempt()
	e.apply(other, &payments.AttemptUpdate{Event: payments.Send})
	e.apply(other, &payments.AttemptUpdate{Event: payments.Accept, ExternalID: "ch_2", BRCode: "br"})
	e.apply(other, &payments.AttemptUpdate{Event: payments.Succeed, ExternalID: "ch_2", Amount: amount, PaidAt: t0})
	before := e.balance("divergence:BRL")

	more := received(other.String(), "ch_2", money.New(amount.Amount()+1, amount.Currency()))
	want := reconciliationapp.Report{Divergences: 2, Opened: 2}
	if rep := e.reconcile(more); rep != want {
		t.Fatalf("report = %+v, want %+v", rep, want)
	}
	if rep := e.reconcile(more); rep != (reconciliationapp.Report{Divergences: 2}) {
		t.Errorf("second run = %+v, want the same two, already open", rep)
	}
	got := e.cases()
	if len(got) != 2 || got["missing_at_provider"] || got["amount_differs"] || !hasKeys(got, "missing_at_provider", "amount_differs") {
		t.Errorf("cases = %v, want missing_at_provider and amount_differs without postings", got)
	}
	if e.balance("divergence:BRL") != before {
		t.Error("a case for a person moved money")
	}
}

func hasKeys(m map[string]bool, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}

// An attempt another provider holds is not this provider's to confirm: for a person,
// with no money moved.
func TestReconciliationLeavesAnotherProvidersAttempt(t *testing.T) {
	e := newInboxEnv(t)
	id, amount := e.attempt()
	if _, err := e.pool.Exec(t.Context(), "UPDATE payment_attempts SET provider = 'cardsim'"); err != nil {
		t.Fatal(err)
	}
	if rep := e.reconcile(received(id.String(), "ch_1", amount)); rep != (reconciliationapp.Report{Divergences: 1, Opened: 1}) {
		t.Fatalf("report = %+v, want one case", rep)
	}
	if got := e.cases(); len(got) != 1 || got["missing_in_system"] || !hasKeys(got, "missing_in_system") {
		t.Errorf("cases = %v, want missing_in_system without a posting", got)
	}
	if got := e.balance("divergence:BRL"); got != 0 {
		t.Errorf("divergence:BRL = %d, want nothing held", got)
	}
}

// failingOnce is a statement whose 2026-10-30 fails the first time it is read, so the
// days after it still run.
type failingOnce struct {
	fixedStatement
	failed atomic.Bool
}

func (f *failingOnce) Statement(ctx context.Context, date calendar.Date) (simkit.Statement, error) {
	if date.String() == "2026-10-30" && f.failed.CompareAndSwap(false, true) {
		return simkit.Statement{}, errors.New("provider down")
	}
	return f.fixedStatement.Statement(ctx, date)
}

func (e *inboxEnv) runDue(r *reconciliationapp.Reconciler) (int, error) {
	return r.RunDue(e.t.Context(), reconciliationadapters.NewRuns(e.pool), []string{"pixsim"})
}

// Decision 6: four workers on the same days reconcile each once, and running again
// does nothing.
func TestDailyReconciliationRunsEachDayOnce(t *testing.T) {
	e := newInboxEnv(t)
	e.clock.Advance(24 * time.Hour)
	r := e.reconciler(fixedStatement{received("someone-else", "ch_1", money.New(500, money.BRL))})

	var wg sync.WaitGroup
	var done atomic.Int64
	for range 4 {
		wg.Go(func() {
			n, err := e.runDue(r)
			if err != nil {
				t.Errorf("RunDue() error = %v", err)
			}
			done.Add(int64(n))
		})
	}
	wg.Wait()
	if n := done.Load(); n != reconciliationapp.Window {
		t.Fatalf("%d days reconciled by four workers, want %d", n, reconciliationapp.Window)
	}
	e.clock.Advance(reconciliationapp.RunLease)
	if n, err := e.runDue(r); n != 0 || err != nil {
		t.Errorf("RunDue() again = %d, %v; want nothing left, finished days stay done", n, err)
	}
	var finished, divergences, settled int
	var last time.Time
	err := e.pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE finished_at IS NOT NULL), sum(divergences), sum(settled), max(day)
		FROM reconciliation_runs WHERE provider = 'pixsim'`).Scan(&finished, &divergences, &settled, &last)
	if err != nil || finished != reconciliationapp.Window || divergences != 1 || settled != 1 {
		t.Errorf("runs: %d finished, %d divergences, %d settled (%v); want %d, 1, 1", finished, divergences, settled, err, reconciliationapp.Window)
	}
	if got := last.Format(time.DateOnly); got != "2026-11-01" {
		t.Errorf("last day reconciled = %s, want 2026-11-01: today is not closed yet", got)
	}
	if got := e.balance("divergence:BRL"); got != -500 {
		t.Errorf("divergence:BRL = %d, want 500 held once", got)
	}
}

// A day whose statement failed is taken again once its lease ends, not before.
func TestDailyReconciliationRetriesAFailedDay(t *testing.T) {
	e := newInboxEnv(t)
	e.clock.Advance(24 * time.Hour)
	r := e.reconciler(&failingOnce{fixedStatement: fixedStatement{received("someone-else", "ch_1", money.New(500, money.BRL))}})

	if n, err := e.runDue(r); n != reconciliationapp.Window-1 || err == nil {
		t.Fatalf("RunDue() = %d, %v; want the other days and the error", n, err)
	}
	if n, err := e.runDue(r); n != 0 || err != nil {
		t.Fatalf("RunDue() within the lease = %d, %v; want nothing", n, err)
	}
	e.clock.Advance(reconciliationapp.RunLease)
	if n, err := e.runDue(r); n != 1 || err != nil {
		t.Fatalf("RunDue() after the lease = %d, %v; want the failed day", n, err)
	}
	if got := e.balance("divergence:BRL"); got != -500 {
		t.Errorf("divergence:BRL = %d, want 500 held", got)
	}
}
