package adapters_test

import (
	"context"
	"errors"
	"flag"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

// checkInvariants asks the database itself, not the Go code that wrote it, whether
// invariants 1, 3, 5 and 6 of section 3.3 hold over every entry ever posted.
func checkInvariants(ctx context.Context, pool *pgxpool.Pool) error {
	checks := []struct {
		invariant string
		query     string
	}{
		{"1: every transaction sums to zero per currency", `
			SELECT count(*) FROM (
				SELECT 1 FROM ledger_entries
				GROUP BY transaction_id, currency
				HAVING sum(amount) <> 0
			) broken`},
		{"3: no must-stay-non-negative account is negative", `
			SELECT count(*) FROM (
				SELECT 1 FROM ledger_accounts a JOIN ledger_entries e ON e.account_id = a.id
				WHERE a.balance_rule = 'must_stay_non_negative'
				GROUP BY a.id
				HAVING sum(e.amount) < 0
			) broken`},
		{"5: every entry is in its account's currency", `
			SELECT count(*) FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
			WHERE e.currency <> a.currency`},
		{"6: all accounts together sum to zero per currency", `
			SELECT count(*) FROM (
				SELECT 1 FROM ledger_entries
				GROUP BY currency
				HAVING sum(amount) <> 0
			) broken`},
	}
	for _, c := range checks {
		var broken int
		if err := pool.QueryRow(ctx, c.query).Scan(&broken); err != nil {
			return err
		}
		if broken != 0 {
			return errors.New("invariant " + c.invariant + " is broken")
		}
	}
	return nil
}

type propertyAccount struct {
	id       domain.AccountID
	currency money.Currency
	rule     domain.BalanceRule
}

// Random sequences of transactions, valid and invalid, against real Postgres. After
// every step each balance must match an in-memory model and the invariants must hold.
func TestLedgerInvariantsAgainstPostgres(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := t.Context()

	rapid.Check(t, func(t *rapid.T) {
		var accounts []propertyAccount
		model := map[domain.AccountID]int64{}
		for range rapid.IntRange(2, 6).Draw(t, "accounts") {
			c := rapid.SampledFrom([]money.Currency{money.USD, money.BRL}).Draw(t, "currency")
			rule := rapid.SampledFrom([]domain.BalanceRule{domain.MayGoNegative, domain.MustStayNonNegative}).Draw(t, "rule")
			a, err := ledger.CreateAccount(ctx, pool, "", c, rule)
			if err != nil {
				t.Fatal(err)
			}
			accounts = append(accounts, propertyAccount{a.ID(), c, rule})
			model[a.ID()] = 0
		}

		for range rapid.IntRange(1, 20).Draw(t, "transactions") {
			var entries []domain.Entry
			for range rapid.IntRange(2, 4).Draw(t, "entries") {
				a := rapid.SampledFrom(accounts).Draw(t, "account")
				// Mostly the account's currency; sometimes the other one, to exercise
				// invariant 5.
				c := a.currency
				if rapid.IntRange(0, 9).Draw(t, "wrong currency") == 0 {
					c = map[money.Currency]money.Currency{money.USD: money.BRL, money.BRL: money.USD}[c]
				}
				amount := rapid.Int64Range(-10_000, 10_000).Filter(func(n int64) bool { return n != 0 }).Draw(t, "amount")
				entries = append(entries, domain.NewEntry(a.id, money.New(amount, c)))
			}
			// Usually close the transaction so it balances; sometimes leave it as drawn.
			if rapid.IntRange(0, 4).Draw(t, "leave unbalanced") != 0 {
				entries = closeEntries(entries, rapid.SampledFrom(accounts).Draw(t, "closing account").id)
			}

			want := expected(entries, accounts, model)
			err := post(ctx, pool, "", entries...)
			switch {
			case want != nil && err == nil:
				t.Fatalf("Post accepted %v, want %v", entries, want)
			case want == nil && err != nil:
				t.Fatalf("Post rejected %v: %v", entries, err)
			case want == nil:
				for _, e := range entries {
					model[e.Account()] += e.Amount().Amount()
				}
			}

			for _, a := range accounts {
				got, err := ledger.Balance(ctx, pool, a.id)
				if err != nil {
					t.Fatal(err)
				}
				if got.Amount() != model[a.id] {
					t.Fatalf("account %s = %v, model says %d", a.id, got, model[a.id])
				}
			}
			if err := checkInvariants(ctx, pool); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// closeEntries appends, per currency, the entry that makes the transaction sum to zero.
// It is a no-op for currencies that already sum to zero.
func closeEntries(entries []domain.Entry, closing domain.AccountID) []domain.Entry {
	sums := map[money.Currency]int64{}
	var order []money.Currency
	for _, e := range entries {
		c := e.Amount().Currency()
		if _, seen := sums[c]; !seen {
			order = append(order, c)
		}
		sums[c] += e.Amount().Amount()
	}
	for _, c := range order {
		if sums[c] != 0 {
			entries = append(entries, domain.NewEntry(closing, money.New(-sums[c], c)))
		}
	}
	return entries
}

// expected is the model's verdict: nil if the ledger must accept the entries, or the
// error it must reject them with.
func expected(entries []domain.Entry, accounts []propertyAccount, model map[domain.AccountID]int64) error {
	if _, err := domain.NewTransaction("", entries...); err != nil {
		return err
	}
	byID := map[domain.AccountID]propertyAccount{}
	for _, a := range accounts {
		byID[a.id] = a
	}
	after := map[domain.AccountID]int64{}
	for _, e := range entries {
		if e.Amount().Currency() != byID[e.Account()].currency {
			return money.ErrCurrencyMismatch
		}
		if _, ok := after[e.Account()]; !ok {
			after[e.Account()] = model[e.Account()]
		}
		after[e.Account()] += e.Amount().Amount()
	}
	for id, b := range after {
		if byID[id].rule == domain.MustStayNonNegative && b < 0 {
			return domain.ErrNegativeBalance
		}
	}
	return nil
}

var seed = flag.Uint64("ledger.seed", 0, "seed for TestThousandsOfConcurrentTransfers; 0 picks one")

// Section 9, cited test 3: thousands of transfers between random accounts, posted by
// concurrent workers. At the end the ledger closes at zero per currency and no
// must-stay-non-negative account is negative.
func TestThousandsOfConcurrentTransfers(t *testing.T) {
	const (
		accountsPerCurrency = 10
		workers             = 10
		transfers           = 5_000
		initialFunds        = 10_000
	)
	pool := pgtest.NewPool(t)
	ctx := t.Context()

	s := *seed
	if s == 0 {
		s = uint64(time.Now().UnixNano())
	}
	t.Logf("seed %d (rerun with -ledger.seed=%d)", s, s)
	rng := rand.New(rand.NewPCG(s, s)) //nolint:gosec // G404: a seeded generator makes failures reproducible; crypto/rand cannot be seeded.

	accounts := map[money.Currency][]domain.AccountID{}
	for _, c := range []money.Currency{money.USD, money.BRL} {
		world := createAccount(t, pool, c, domain.MayGoNegative)
		for i := range accountsPerCurrency {
			rule := domain.MustStayNonNegative
			if i%3 == 0 {
				rule = domain.MayGoNegative
			}
			id := createAccount(t, pool, c, rule)
			if err := post(ctx, pool, "", domain.NewEntry(world, money.New(-initialFunds, c)), domain.NewEntry(id, money.New(initialFunds, c))); err != nil {
				t.Fatal(err)
			}
			accounts[c] = append(accounts[c], id)
		}
	}

	type transfer struct {
		from, to domain.AccountID
		amount   money.Money
	}
	plan := make([]transfer, transfers)
	for i := range plan {
		c := []money.Currency{money.USD, money.BRL}[rng.IntN(2)]
		ids := accounts[c]
		from := ids[rng.IntN(len(ids))]
		to := ids[rng.IntN(len(ids))]
		for to == from {
			to = ids[rng.IntN(len(ids))]
		}
		plan[i] = transfer{from, to, money.New(rng.Int64N(initialFunds/2)+1, c)}
	}

	var next, ok, rejected atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for {
				i := next.Add(1) - 1
				if i >= transfers {
					return
				}
				p := plan[i]
				neg, err := p.amount.Neg()
				if err != nil {
					t.Error(err)
					return
				}
				err = post(ctx, pool, "", domain.NewEntry(p.from, neg), domain.NewEntry(p.to, p.amount))
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, domain.ErrNegativeBalance):
					rejected.Add(1)
				default:
					t.Errorf("transfer %d: %v", i, err)
					return
				}
			}
		})
	}
	wg.Wait()

	t.Logf("%d transfers: %d posted, %d rejected for insufficient balance", transfers, ok.Load(), rejected.Load())
	if ok.Load()+rejected.Load() != transfers {
		t.Fatalf("accounted for %d of %d transfers", ok.Load()+rejected.Load(), transfers)
	}
	if ok.Load() == 0 || rejected.Load() == 0 {
		t.Errorf("want both posted and rejected transfers to exercise the lock and the balance check")
	}

	var posted int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ledger_transactions").Scan(&posted); err != nil {
		t.Fatal(err)
	}
	// One funding transaction per account plus every successful transfer.
	if want := ok.Load() + 2*accountsPerCurrency; posted != want {
		t.Errorf("%d transactions in the ledger, want %d", posted, want)
	}
	if err := checkInvariants(ctx, pool); err != nil {
		t.Error(err)
	}
}
