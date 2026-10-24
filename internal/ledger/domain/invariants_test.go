package domain_test

import (
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

// model is an in-memory ledger: every account starts at zero and only Apply moves it.
// The database version of this test arrives with the adapter.
type model struct {
	states   map[domain.AccountID]domain.AccountState
	accounts []domain.Account
}

func newModel(t *rapid.T) *model {
	m := &model{states: make(map[domain.AccountID]domain.AccountState)}
	n := rapid.IntRange(2, 8).Draw(t, "accounts")
	for i := range n {
		c := rapid.SampledFrom([]money.Currency{money.USD, money.BRL}).Draw(t, "currency")
		rule := rapid.SampledFrom([]domain.BalanceRule{domain.MayGoNegative, domain.MustStayNonNegative}).Draw(t, "rule")
		a, err := domain.NewAccount(id(byte(i+1)), c, rule)
		if err != nil {
			t.Fatal(err)
		}
		m.accounts = append(m.accounts, a)
		m.states[a.ID()] = domain.AccountState{Account: a, Balance: money.Zero(c)}
	}
	return m
}

// drawTransaction builds entries on random accounts of one currency and closes them
// with a balancing entry, so NewTransaction must accept it.
func (m *model) drawTransaction(t *rapid.T) (domain.Transaction, bool) {
	c := rapid.SampledFrom([]money.Currency{money.USD, money.BRL}).Draw(t, "tx currency")
	var same []domain.Account
	for _, a := range m.accounts {
		if a.Currency() == c {
			same = append(same, a)
		}
	}
	if len(same) < 2 {
		return domain.Transaction{}, false
	}

	var entries []domain.Entry
	var sum int64
	for range rapid.IntRange(1, 4).Draw(t, "entries") {
		a := rapid.SampledFrom(same).Draw(t, "account")
		amount := rapid.Int64Range(-1_000_000, 1_000_000).Filter(func(n int64) bool { return n != 0 }).Draw(t, "amount")
		entries = append(entries, domain.NewEntry(a.ID(), money.New(amount, c)))
		sum += amount
	}
	if sum == 0 {
		return domain.Transaction{}, false
	}
	closing := rapid.SampledFrom(same).Draw(t, "closing account")
	entries = append(entries, domain.NewEntry(closing.ID(), money.New(-sum, c)))

	tx, err := domain.NewTransaction("", entries...)
	if err != nil {
		t.Fatalf("balanced entries rejected: %v", err)
	}
	return tx, true
}

func (m *model) apply(t *rapid.T, tx domain.Transaction) {
	after, err := tx.Apply(m.states)
	if errors.Is(err, domain.ErrNegativeBalance) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for id, balance := range after {
		s := m.states[id]
		s.Balance = balance
		m.states[id] = s
	}
}

// Invariants 1, 3, 5 and 6 over random sequences of transactions.
func TestLedgerInvariants(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := newModel(t)
		for range rapid.IntRange(1, 50).Draw(t, "transactions") {
			tx, ok := m.drawTransaction(t)
			if !ok {
				continue
			}
			m.apply(t, tx)

			totals := map[money.Currency]int64{}
			for _, s := range m.states {
				if s.Balance.Currency() != s.Account.Currency() {
					t.Fatalf("invariant 5: account %s in %s holds %s", s.Account.ID(), s.Account.Currency(), s.Balance)
				}
				if s.Account.Rule() == domain.MustStayNonNegative && s.Balance.IsNegative() {
					t.Fatalf("invariant 3: account %s is %s", s.Account.ID(), s.Balance)
				}
				totals[s.Balance.Currency()] += s.Balance.Amount()
			}
			for c, total := range totals {
				if total != 0 {
					t.Fatalf("invariant 6: %s sums to %d", c, total)
				}
			}
		}
	})
}

func TestUnbalancedIsAlwaysRejected(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := newModel(t)
		tx, ok := m.drawTransaction(t)
		if !ok {
			t.Skip("not enough accounts in one currency")
		}
		entries := tx.Entries()
		i := rapid.IntRange(0, len(entries)-1).Draw(t, "entry")
		delta := rapid.SampledFrom([]int64{-1, 1}).Draw(t, "delta")
		e := entries[i]
		entries[i] = domain.NewEntry(e.Account(), money.New(e.Amount().Amount()+delta, e.Amount().Currency()))

		if _, err := domain.NewTransaction("", entries...); err == nil {
			t.Fatalf("NewTransaction accepted entries off by %d", delta)
		}
	})
}
