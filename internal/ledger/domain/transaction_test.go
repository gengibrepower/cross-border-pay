package domain_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

func id(n byte) domain.AccountID { return domain.AccountID{15: n} }

func usd(n int64) money.Money { return money.New(n, money.USD) }

func brl(n int64) money.Money { return money.New(n, money.BRL) }

func entry(account byte, m money.Money) domain.Entry { return domain.NewEntry(id(account), m) }

func TestNewTransaction(t *testing.T) {
	tests := []struct {
		name    string
		entries []domain.Entry
		wantErr error
	}{
		{
			name: "GameStore confirmation",
			entries: []domain.Entry{
				entry(1, brl(10150)), entry(2, brl(-10150)),
				entry(3, usd(-2030)), entry(4, usd(1940)), entry(5, usd(60)), entry(6, usd(30)),
			},
		},
		{name: "two entries", entries: []domain.Entry{entry(1, usd(5)), entry(2, usd(-5))}},
		{name: "same account twice", entries: []domain.Entry{entry(1, usd(5)), entry(1, usd(-5))}},
		{name: "unbalanced", entries: []domain.Entry{entry(1, usd(5)), entry(2, usd(-4))}, wantErr: domain.ErrUnbalanced},
		{
			name:    "balanced in total but not per currency",
			entries: []domain.Entry{entry(1, usd(5)), entry(2, brl(-5))},
			wantErr: domain.ErrUnbalanced,
		},
		{name: "single entry", entries: []domain.Entry{entry(1, usd(0))}, wantErr: domain.ErrTooFewEntries},
		{name: "no entries", wantErr: domain.ErrTooFewEntries},
		{
			name:    "zero entry",
			entries: []domain.Entry{entry(1, usd(5)), entry(2, usd(-5)), entry(3, usd(0))},
			wantErr: domain.ErrZeroEntry,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := domain.NewTransaction("", tt.entries...)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("NewTransaction() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestTransactionEntriesAreACopy(t *testing.T) {
	entries := []domain.Entry{entry(1, usd(5)), entry(2, usd(-5))}
	tx, err := domain.NewTransaction("", entries...)
	if err != nil {
		t.Fatal(err)
	}

	entries[0] = entry(1, usd(999))
	got := tx.Entries()
	got[1] = entry(2, usd(999))

	if want := []domain.Entry{entry(1, usd(5)), entry(2, usd(-5))}; !slices.Equal(tx.Entries(), want) {
		t.Errorf("Entries() = %v after outside edits, want %v", tx.Entries(), want)
	}
}

func TestLockOrder(t *testing.T) {
	tx, err := domain.NewTransaction("",
		entry(9, usd(5)), entry(2, usd(-3)), entry(9, usd(1)), entry(4, usd(-3)))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tx.LockOrder(), []domain.AccountID{id(2), id(4), id(9)}; !slices.Equal(got, want) {
		t.Errorf("LockOrder() = %v, want %v", got, want)
	}
}

func account(t *testing.T, n byte, c money.Currency, rule domain.BalanceRule) domain.Account {
	t.Helper()
	a, err := domain.NewAccount(id(n), c, rule)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestApply(t *testing.T) {
	states := map[domain.AccountID]domain.AccountState{
		id(1): {Account: account(t, 1, money.USD, domain.MustStayNonNegative), Balance: usd(100)},
		id(2): {Account: account(t, 2, money.USD, domain.MayGoNegative), Balance: usd(0)},
		id(3): {Account: account(t, 3, money.BRL, domain.MayGoNegative), Balance: brl(0)},
	}
	tests := []struct {
		name    string
		entries []domain.Entry
		want    map[domain.AccountID]money.Money
		wantErr error
	}{
		{
			name:    "down to exactly zero",
			entries: []domain.Entry{entry(1, usd(-100)), entry(2, usd(100))},
			want:    map[domain.AccountID]money.Money{id(1): usd(0), id(2): usd(100)},
		},
		{
			name:    "below zero on a non-negative account",
			entries: []domain.Entry{entry(1, usd(-101)), entry(2, usd(101))},
			wantErr: domain.ErrNegativeBalance,
		},
		{
			name:    "may-go-negative account goes negative",
			entries: []domain.Entry{entry(2, usd(-500)), entry(1, usd(500))},
			want:    map[domain.AccountID]money.Money{id(1): usd(600), id(2): usd(-500)},
		},
		{
			name:    "net effect counts, not entry order",
			entries: []domain.Entry{entry(1, usd(-150)), entry(1, usd(60)), entry(2, usd(90))},
			want:    map[domain.AccountID]money.Money{id(1): usd(10), id(2): usd(90)},
		},
		{
			name:    "currency differs from account",
			entries: []domain.Entry{entry(3, usd(5)), entry(2, usd(-5))},
			wantErr: money.ErrCurrencyMismatch,
		},
		{
			name:    "account not locked",
			entries: []domain.Entry{entry(7, usd(5)), entry(2, usd(-5))},
			wantErr: domain.ErrUnknownAccount,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := domain.NewTransaction("", tt.entries...)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tx.Apply(states)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Apply() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && !mapsEqual(got, tt.want) {
				t.Errorf("Apply() = %v, want %v", got, tt.want)
			}
		})
	}
}

func mapsEqual(a, b map[domain.AccountID]money.Money) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func TestNewAccountRejects(t *testing.T) {
	if _, err := domain.NewAccount(id(1), money.Currency{}, domain.MayGoNegative); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Errorf("no currency: error = %v, want %v", err, domain.ErrInvalidAccount)
	}
	if _, err := domain.NewAccount(id(1), money.USD, 0); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Errorf("zero rule: error = %v, want %v", err, domain.ErrInvalidAccount)
	}
}

func TestAccountIDString(t *testing.T) {
	got := domain.AccountID{0x01, 0x9a, 15: 0xff}.String()
	if want := "019a0000-0000-0000-0000-0000000000ff"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
