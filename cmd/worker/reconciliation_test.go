package main

import (
	"context"
	"testing"

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

func (e *inboxEnv) reconcile(movements ...simkit.Movement) reconciliationapp.Report {
	e.t.Helper()
	cal, err := calendar.Brazil()
	if err != nil {
		e.t.Fatal(err)
	}
	r := reconciliationapp.NewReconciler(statements{"pixsim": fixedStatement(movements)}, books{e.payments},
		reconciliationadapters.NewCases(e.pool), &cal, e.clock)
	rep, err := r.Reconcile(e.t.Context(), "pixsim", cal.Today(t0))
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
