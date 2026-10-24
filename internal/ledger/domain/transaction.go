package domain

import (
	"errors"
	"fmt"
	"slices"

	"github.com/gengibrepower/cross-border-pay/internal/money"
)

var (
	ErrUnbalanced      = errors.New("ledger: entries do not sum to zero")
	ErrTooFewEntries   = errors.New("ledger: a transaction needs at least two entries")
	ErrZeroEntry       = errors.New("ledger: entry with zero amount")
	ErrUnknownAccount  = errors.New("ledger: unknown account")
	ErrNegativeBalance = errors.New("ledger: balance would go negative")
)

// Entry is a signed amount on one account: positive and negative entries of the same
// currency cancel out inside a transaction (section 3.3).
type Entry struct {
	account AccountID
	amount  money.Money
}

func NewEntry(account AccountID, amount money.Money) Entry {
	return Entry{account: account, amount: amount}
}

func (e Entry) Account() AccountID { return e.account }

func (e Entry) Amount() money.Money { return e.amount }

type Transaction struct {
	reference string
	entries   []Entry
}

// NewTransaction checks invariant 1: per currency, the entries sum to zero. A
// non-empty reference is unique in the ledger, so posting it twice fails.
func NewTransaction(reference string, entries ...Entry) (Transaction, error) {
	if len(entries) < 2 {
		return Transaction{}, fmt.Errorf("%w: got %d", ErrTooFewEntries, len(entries))
	}

	sums := make(map[money.Currency]money.Money)
	for _, e := range entries {
		if e.amount.IsZero() {
			return Transaction{}, fmt.Errorf("%w: account %s", ErrZeroEntry, e.account)
		}
		c := e.amount.Currency()
		sum, ok := sums[c]
		if !ok {
			sum = money.Zero(c)
		}
		next, err := sum.Add(e.amount)
		if err != nil {
			return Transaction{}, fmt.Errorf("ledger: summing %s: %w", c, err)
		}
		sums[c] = next
	}
	for c, sum := range sums {
		if !sum.IsZero() {
			return Transaction{}, fmt.Errorf("%w: %s sums to %s", ErrUnbalanced, c, sum)
		}
	}

	return Transaction{reference: reference, entries: slices.Clone(entries)}, nil
}

func (t Transaction) Reference() string { return t.reference }

// Entries returns a copy, so callers cannot edit a validated transaction.
func (t Transaction) Entries() []Entry { return slices.Clone(t.entries) }

// LockOrder lists each account once, ascending, the order the database locks them in
// so that two transactions over the same accounts cannot deadlock (decision 10b).
func (t Transaction) LockOrder() []AccountID {
	ids := make([]AccountID, 0, len(t.entries))
	for _, e := range t.entries {
		ids = append(ids, e.account)
	}
	slices.SortFunc(ids, AccountID.Compare)
	return slices.Compact(ids)
}

type AccountState struct {
	Account Account
	Balance money.Money
}

// Apply checks the transaction against the accounts it touches, as locked and summed
// by the caller, and returns their balances afterwards. It enforces invariant 5 (an
// entry's currency is its account's) and invariant 3 (MustStayNonNegative accounts
// never go below zero).
func (t Transaction) Apply(states map[AccountID]AccountState) (map[AccountID]money.Money, error) {
	after := make(map[AccountID]money.Money, len(states))
	for _, e := range t.entries {
		state, ok := states[e.account]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownAccount, e.account)
		}
		if e.amount.Currency() != state.Account.currency {
			return nil, fmt.Errorf("ledger: %s on account %s in %s: %w",
				e.amount, e.account, state.Account.currency, money.ErrCurrencyMismatch)
		}
		balance, seen := after[e.account]
		if !seen {
			balance = state.Balance
		}
		next, err := balance.Add(e.amount)
		if err != nil {
			return nil, fmt.Errorf("ledger: balance of %s: %w", e.account, err)
		}
		after[e.account] = next
	}

	for id, balance := range after {
		if states[id].Account.rule == MustStayNonNegative && balance.IsNegative() {
			return nil, fmt.Errorf("%w: account %s would be %s", ErrNegativeBalance, id, balance)
		}
	}
	return after, nil
}
