package adapters_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/ledger/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var ledger adapters.Ledger

func createAccount(t *testing.T, pool *pgxpool.Pool, c money.Currency, rule domain.BalanceRule) domain.AccountID {
	t.Helper()
	a, err := ledger.CreateAccount(t.Context(), pool, "", c, rule)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID()
}

func post(ctx context.Context, pool *pgxpool.Pool, reference string, entries ...domain.Entry) error {
	tx, err := domain.NewTransaction(reference, entries...)
	if err != nil {
		return err
	}
	return postgres.WithTx(ctx, pool, pgx.ReadCommitted, func(ctx context.Context, dbtx pgx.Tx) error {
		_, err := ledger.Post(ctx, dbtx, tx)
		return err
	})
}

func balance(t *testing.T, pool *pgxpool.Pool, id domain.AccountID) money.Money {
	t.Helper()
	b, err := ledger.Balance(context.WithoutCancel(t.Context()), pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func usd(n int64) money.Money { return money.New(n, money.USD) }

func brl(n int64) money.Money { return money.New(n, money.BRL) }

// Section 3.3: the GameStore confirmation, posted and read back.
func TestPostGameStore(t *testing.T) {
	pool := pgtest.NewPool(t)
	received := createAccount(t, pool, money.BRL, domain.MayGoNegative)
	fxBRL := createAccount(t, pool, money.BRL, domain.MayGoNegative)
	fxUSD := createAccount(t, pool, money.USD, domain.MayGoNegative)
	merchant := createAccount(t, pool, money.USD, domain.MustStayNonNegative)
	fees := createAccount(t, pool, money.USD, domain.MustStayNonNegative)
	spread := createAccount(t, pool, money.USD, domain.MustStayNonNegative)

	err := post(t.Context(), pool, "payment:1:confirmation",
		domain.NewEntry(received, brl(10150)), domain.NewEntry(fxBRL, brl(-10150)),
		domain.NewEntry(fxUSD, usd(-2030)), domain.NewEntry(merchant, usd(1940)),
		domain.NewEntry(fees, usd(60)), domain.NewEntry(spread, usd(30)))
	if err != nil {
		t.Fatal(err)
	}

	want := map[domain.AccountID]money.Money{
		received: brl(10150), fxBRL: brl(-10150), fxUSD: usd(-2030),
		merchant: usd(1940), fees: usd(60), spread: usd(30),
	}
	for id, w := range want {
		if got := balance(t, pool, id); got != w {
			t.Errorf("balance(%s) = %v, want %v", id, got, w)
		}
	}
}

func TestPostRejections(t *testing.T) {
	pool := pgtest.NewPool(t)
	world := createAccount(t, pool, money.USD, domain.MayGoNegative)
	wallet := createAccount(t, pool, money.USD, domain.MustStayNonNegative)
	reais := createAccount(t, pool, money.BRL, domain.MayGoNegative)
	if err := post(t.Context(), pool, "fund", domain.NewEntry(world, usd(-100)), domain.NewEntry(wallet, usd(100))); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		reference string
		entries   []domain.Entry
		wantErr   error
	}{
		{
			name:    "overdraw a non-negative account",
			entries: []domain.Entry{domain.NewEntry(wallet, usd(-101)), domain.NewEntry(world, usd(101))},
			wantErr: domain.ErrNegativeBalance,
		},
		{
			name:    "entry currency differs from account",
			entries: []domain.Entry{domain.NewEntry(reais, usd(-1)), domain.NewEntry(world, usd(1))},
			wantErr: money.ErrCurrencyMismatch,
		},
		{
			name:    "account does not exist",
			entries: []domain.Entry{domain.NewEntry(domain.AccountID{1}, usd(-1)), domain.NewEntry(world, usd(1))},
			wantErr: domain.ErrUnknownAccount,
		},
		{
			name:      "reference already posted",
			reference: "fund",
			entries:   []domain.Entry{domain.NewEntry(world, usd(-1)), domain.NewEntry(wallet, usd(1))},
			wantErr:   domain.ErrDuplicateReference,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := post(t.Context(), pool, tt.reference, tt.entries...)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Post() error = %v, want %v", err, tt.wantErr)
			}
			if got := balance(t, pool, wallet); got != usd(100) {
				t.Errorf("wallet = %v after a rejected post, want 1.00 USD", got)
			}
		})
	}
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// Invariant 2, enforced by the database even if Go code tries.
func TestEntriesAreImmutable(t *testing.T) {
	pool := pgtest.NewPool(t)
	a := createAccount(t, pool, money.USD, domain.MayGoNegative)
	b := createAccount(t, pool, money.USD, domain.MayGoNegative)
	if err := post(t.Context(), pool, "x", domain.NewEntry(a, usd(-5)), domain.NewEntry(b, usd(5))); err != nil {
		t.Fatal(err)
	}

	statements := []string{
		"UPDATE ledger_entries SET amount = amount * 2",
		"DELETE FROM ledger_entries",
		"TRUNCATE ledger_entries CASCADE",
		"UPDATE ledger_transactions SET reference = 'y'",
		"DELETE FROM ledger_transactions",
		"TRUNCATE ledger_transactions CASCADE",
	}
	for _, sql := range statements {
		if _, err := pool.Exec(t.Context(), sql); pgCode(err) != "23001" {
			t.Errorf("%s: error = %v, want restrict_violation (23001)", sql, err)
		}
	}
	if got := balance(t, pool, b); got != usd(5) {
		t.Errorf("balance = %v, want 0.05 USD", got)
	}
}

// Invariant 5, enforced by the composite foreign key even if Go code skips Apply.
func TestEntryCurrencyMatchesAccountInDatabase(t *testing.T) {
	pool := pgtest.NewPool(t)
	a := createAccount(t, pool, money.BRL, domain.MayGoNegative)

	_, err := pool.Exec(t.Context(), `
		WITH tx AS (INSERT INTO ledger_transactions DEFAULT VALUES RETURNING id)
		INSERT INTO ledger_entries (transaction_id, account_id, currency, amount)
		SELECT id, $1, 'USD', 5 FROM tx`, a[:])
	if pgCode(err) != "23503" {
		t.Errorf("error = %v, want foreign_key_violation (23503)", err)
	}
}

// race opens n transactions, waits until all of them are open and only then posts in
// each, so the posts overlap instead of queueing behind the pool's first connection.
// It skips WithTx on purpose: a retry would hide a deadlock instead of failing on it.
func race(t *testing.T, pool *pgxpool.Pool, txs []domain.Transaction) []error {
	t.Helper()
	if len(txs) >= int(pool.Config().MaxConns) {
		t.Fatalf("%d transactions need more connections than the pool's %d", len(txs), pool.Config().MaxConns)
	}
	errs := make([]error, len(txs))
	var opened, done sync.WaitGroup
	start := make(chan struct{})
	opened.Add(len(txs))
	for i, tx := range txs {
		done.Go(func() {
			dbtx, err := pool.Begin(t.Context())
			opened.Done()
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = dbtx.Rollback(context.WithoutCancel(t.Context())) }()
			<-start
			if _, err := ledger.Post(t.Context(), dbtx, tx); err != nil {
				errs[i] = err
				return
			}
			errs[i] = dbtx.Commit(t.Context())
		})
	}
	opened.Wait()
	close(start)
	done.Wait()
	return errs
}

func transfers(t *testing.T, n int, from, to func(i int) domain.AccountID, amount money.Money) []domain.Transaction {
	t.Helper()
	neg, err := amount.Neg()
	if err != nil {
		t.Fatal(err)
	}
	txs := make([]domain.Transaction, n)
	for i := range n {
		if txs[i], err = domain.NewTransaction("", domain.NewEntry(from(i), neg), domain.NewEntry(to(i), amount)); err != nil {
			t.Fatal(err)
		}
	}
	return txs
}

// Invariant 3 under concurrency: 10 withdrawals of 30 from 100 race; exactly 3 fit.
func TestConcurrentWithdrawalsNeverOverdraw(t *testing.T) {
	pool := pgtest.NewPool(t)
	world := createAccount(t, pool, money.USD, domain.MayGoNegative)
	wallet := createAccount(t, pool, money.USD, domain.MustStayNonNegative)
	if err := post(t.Context(), pool, "", domain.NewEntry(world, usd(-100)), domain.NewEntry(wallet, usd(100))); err != nil {
		t.Fatal(err)
	}

	always := func(id domain.AccountID) func(int) domain.AccountID { return func(int) domain.AccountID { return id } }
	var ok, rejected int
	for _, err := range race(t, pool, transfers(t, 10, always(wallet), always(world), usd(30))) {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrNegativeBalance):
			rejected++
		default:
			t.Error(err)
		}
	}

	if ok != 3 || rejected != 7 {
		t.Errorf("succeeded %d, rejected %d; want 3 and 7", ok, rejected)
	}
	if got := balance(t, pool, wallet); got != usd(10) {
		t.Errorf("wallet = %v, want 0.10 USD", got)
	}
}

// Transfers in opposite directions over the same two accounts would deadlock if each
// locked its debit account first; locking in ID order means none ever does.
func TestOppositeTransfersDoNotDeadlock(t *testing.T) {
	pool := pgtest.NewPool(t)
	a := createAccount(t, pool, money.USD, domain.MayGoNegative)
	b := createAccount(t, pool, money.USD, domain.MayGoNegative)

	pick := func(even, odd domain.AccountID) func(int) domain.AccountID {
		return func(i int) domain.AccountID {
			if i%2 == 0 {
				return even
			}
			return odd
		}
	}
	for i, err := range race(t, pool, transfers(t, 10, pick(a, b), pick(b, a), usd(1))) {
		if err != nil {
			t.Errorf("transfer %d: %v", i, err)
		}
	}

	if got := balance(t, pool, a); got != usd(0) {
		t.Errorf("a = %v, want 0.00 USD", got)
	}
}

// The migration's chart of accounts (section 3.3) is there, each in its currency.
func TestChartOfAccounts(t *testing.T) {
	pool := pgtest.NewPool(t)
	want := map[string]money.Currency{
		"provider_receivables:BRL": money.BRL, "fx:BRL": money.BRL, "fx:USD": money.USD,
		"fee_revenue:USD": money.USD, "spread_revenue:USD": money.USD, "fx_result:USD": money.USD,
		"rounding:BRL": money.BRL, "rounding:USD": money.USD, "divergence:BRL": money.BRL,
		"settlements_paid:USD": money.USD,
	}
	codes := make([]string, 0, len(want))
	for code := range want {
		codes = append(codes, code)
	}
	accounts, err := ledger.AccountsByCode(t.Context(), pool, codes...)
	if err != nil {
		t.Fatal(err)
	}
	for code, c := range want {
		if accounts[code].Currency() != c {
			t.Errorf("%s in %s, want %s", code, accounts[code].Currency().Code(), c.Code())
		}
	}
}

func TestAccountsByCodeNamesTheMissingOne(t *testing.T) {
	pool := pgtest.NewPool(t)
	if _, err := ledger.AccountsByCode(t.Context(), pool, "fx:USD", "nope"); !errors.Is(err, domain.ErrUnknownAccount) {
		t.Errorf("error = %v, want %v", err, domain.ErrUnknownAccount)
	}
}

// Many first confirmations of one merchant race to create its account; one wins and
// all of them get it.
func TestConcurrentEnsureAccountCreatesOne(t *testing.T) {
	pool := pgtest.NewPool(t)
	const callers = 50
	ids := make([]domain.AccountID, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			a, err := ledger.EnsureAccount(t.Context(), pool, "merchant_payable:m1", money.USD, domain.MayGoNegative)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = a.ID()
		})
	}
	wg.Wait()
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("caller %d got %s, caller 0 got %s", i, id, ids[0])
		}
	}
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM ledger_accounts WHERE code = 'merchant_payable:m1'").Scan(&n); err != nil || n != 1 {
		t.Errorf("%d accounts (%v), want 1", n, err)
	}
}

func TestEnsureAccountRefusesAnotherCurrencyOrRule(t *testing.T) {
	pool := pgtest.NewPool(t)
	if _, err := ledger.EnsureAccount(t.Context(), pool, "x", money.USD, domain.MayGoNegative); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		currency money.Currency
		rule     domain.BalanceRule
	}{
		"currency": {money.BRL, domain.MayGoNegative},
		"rule":     {money.USD, domain.MustStayNonNegative},
	} {
		if _, err := ledger.EnsureAccount(t.Context(), pool, "x", tc.currency, tc.rule); !errors.Is(err, domain.ErrInvalidAccount) {
			t.Errorf("another %s: error = %v, want %v", name, err, domain.ErrInvalidAccount)
		}
	}
}
