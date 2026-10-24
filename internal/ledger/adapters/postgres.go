package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gengibrepower/cross-border-pay/internal/ledger/adapters/ledgerdb"
	"github.com/gengibrepower/cross-border-pay/internal/ledger/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
)

// DB is a pool or a transaction; Post needs a transaction because it holds row locks.
type DB = ledgerdb.DBTX

const uniqueViolation = "23505"

const (
	mayGoNegative       = "may_go_negative"
	mustStayNonNegative = "must_stay_non_negative"
)

// Ledger is the Postgres ledger of decision 10. It keeps no state: every method takes
// the connection or transaction to run on, so callers can post inside their own
// transaction together with other writes.
type Ledger struct{}

func (Ledger) CreateAccount(ctx context.Context, db DB, code string, currency money.Currency, rule domain.BalanceRule) (domain.Account, error) {
	dbRule, err := ruleToDB(rule)
	if err != nil {
		return domain.Account{}, err
	}
	id, err := ledgerdb.New(db).CreateAccount(ctx, ledgerdb.CreateAccountParams{
		Code:        pgtype.Text{String: code, Valid: code != ""},
		Currency:    currency.Code(),
		BalanceRule: dbRule,
	})
	if err != nil {
		return domain.Account{}, fmt.Errorf("ledger: create account: %w", err)
	}
	account, err := domain.NewAccount(id.Bytes, currency, rule)
	if err != nil {
		return domain.Account{}, fmt.Errorf("ledger: create account: %w", err)
	}
	return account, nil
}

func (Ledger) Balance(ctx context.Context, db DB, id domain.AccountID) (money.Money, error) {
	q := ledgerdb.New(db)
	row, err := q.GetAccount(ctx, toUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return money.Money{}, fmt.Errorf("%w: %s", domain.ErrUnknownAccount, id)
	}
	if err != nil {
		return money.Money{}, fmt.Errorf("ledger: get account %s: %w", id, err)
	}
	currency, err := money.ParseCurrency(row.Currency)
	if err != nil {
		return money.Money{}, fmt.Errorf("ledger: account %s: %w", id, err)
	}
	balances, err := q.Balances(ctx, []pgtype.UUID{toUUID(id)})
	if err != nil {
		return money.Money{}, fmt.Errorf("ledger: balance of %s: %w", id, err)
	}
	if len(balances) == 0 {
		return money.Zero(currency), nil
	}
	return money.New(balances[0].Balance, currency), nil
}

// Post locks the accounts in ascending order, sums their balances and only then
// checks and writes. Under READ COMMITTED each statement sees what was committed
// before it started, and the lock makes every other writer to these accounts commit
// or roll back first, so the sums are current (decisions 9 and 10b).
func (Ledger) Post(ctx context.Context, tx pgx.Tx, t domain.Transaction) (domain.TransactionID, error) {
	q := ledgerdb.New(tx)
	ids := t.LockOrder()

	states, err := lock(ctx, q, ids)
	if err != nil {
		return domain.TransactionID{}, err
	}
	if _, err := t.Apply(states); err != nil {
		return domain.TransactionID{}, fmt.Errorf("ledger: post %q: %w", t.Reference(), err)
	}

	txID, err := q.InsertTransaction(ctx, pgtype.Text{String: t.Reference(), Valid: t.Reference() != ""})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return domain.TransactionID{}, fmt.Errorf("%w: %q", domain.ErrDuplicateReference, t.Reference())
	}
	if err != nil {
		return domain.TransactionID{}, fmt.Errorf("ledger: insert transaction: %w", err)
	}

	entries := t.Entries()
	params := ledgerdb.InsertEntriesParams{
		TransactionID: txID,
		AccountIds:    make([]pgtype.UUID, len(entries)),
		Currencies:    make([]string, len(entries)),
		Amounts:       make([]int64, len(entries)),
	}
	for i, e := range entries {
		params.AccountIds[i] = toUUID(e.Account())
		params.Currencies[i] = e.Amount().Currency().Code()
		params.Amounts[i] = e.Amount().Amount()
	}
	if err := q.InsertEntries(ctx, params); err != nil {
		return domain.TransactionID{}, fmt.Errorf("ledger: insert entries: %w", err)
	}
	return txID.Bytes, nil
}

func lock(ctx context.Context, q *ledgerdb.Queries, ids []domain.AccountID) (map[domain.AccountID]domain.AccountState, error) {
	uuids := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		uuids[i] = toUUID(id)
	}

	rows, err := q.LockAccounts(ctx, uuids)
	if err != nil {
		return nil, fmt.Errorf("ledger: lock accounts: %w", err)
	}
	balances, err := q.Balances(ctx, uuids)
	if err != nil {
		return nil, fmt.Errorf("ledger: balances: %w", err)
	}
	sums := make(map[domain.AccountID]int64, len(balances))
	for _, b := range balances {
		sums[b.AccountID.Bytes] = b.Balance
	}

	states := make(map[domain.AccountID]domain.AccountState, len(rows))
	for _, row := range rows {
		account, err := accountFromRow(row.ID, row.Currency, row.BalanceRule)
		if err != nil {
			return nil, err
		}
		states[account.ID()] = domain.AccountState{
			Account: account,
			Balance: money.New(sums[account.ID()], account.Currency()),
		}
	}
	return states, nil
}

func accountFromRow(id pgtype.UUID, currencyCode, ruleCode string) (domain.Account, error) {
	currency, err := money.ParseCurrency(currencyCode)
	if err != nil {
		return domain.Account{}, fmt.Errorf("ledger: account %x: %w", id.Bytes, err)
	}
	var rule domain.BalanceRule
	switch ruleCode {
	case mayGoNegative:
		rule = domain.MayGoNegative
	case mustStayNonNegative:
		rule = domain.MustStayNonNegative
	default:
		return domain.Account{}, fmt.Errorf("%w: %x has balance rule %q", domain.ErrInvalidAccount, id.Bytes, ruleCode)
	}
	account, err := domain.NewAccount(id.Bytes, currency, rule)
	if err != nil {
		return domain.Account{}, fmt.Errorf("ledger: account %x: %w", id.Bytes, err)
	}
	return account, nil
}

func ruleToDB(rule domain.BalanceRule) (string, error) {
	switch rule {
	case domain.MayGoNegative:
		return mayGoNegative, nil
	case domain.MustStayNonNegative:
		return mustStayNonNegative, nil
	default:
		return "", fmt.Errorf("%w: unknown balance rule %d", domain.ErrInvalidAccount, rule)
	}
}

func toUUID(id domain.AccountID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
