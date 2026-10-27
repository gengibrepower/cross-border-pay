-- name: CreateAccount :one
INSERT INTO ledger_accounts (code, currency, balance_rule)
VALUES (sqlc.narg(code), @currency, @balance_rule)
RETURNING id;

-- name: GetAccount :one
SELECT id, currency, balance_rule
FROM ledger_accounts
WHERE id = @id;

-- Rows are locked in ORDER BY order, so every transaction takes its accounts' locks
-- ascending and two transactions cannot wait on each other (decision 10b).
-- name: LockAccounts :many
SELECT id, currency, balance_rule
FROM ledger_accounts
WHERE id = ANY(@ids::uuid[])
ORDER BY id
FOR UPDATE;

-- name: Balances :many
SELECT account_id, sum(amount)::bigint AS balance
FROM ledger_entries
WHERE account_id = ANY(@ids::uuid[])
GROUP BY account_id;

-- name: InsertTransaction :one
INSERT INTO ledger_transactions (reference)
VALUES (sqlc.narg(reference))
RETURNING id;

-- name: InsertEntries :exec
INSERT INTO ledger_entries (transaction_id, account_id, currency, amount)
SELECT @transaction_id, unnest(@account_ids::uuid[]), unnest(@currencies::text[]), unnest(@amounts::bigint[]);

-- name: AccountsByCode :many
SELECT id, code, currency, balance_rule
FROM ledger_accounts
WHERE code = ANY(@codes::text[]);

-- name: InsertAccountIfAbsent :exec
-- A concurrent insert of the same code waits for the other transaction; once that one
-- commits, this one does nothing.
INSERT INTO ledger_accounts (code, currency, balance_rule)
VALUES (@code, @currency, @balance_rule)
ON CONFLICT (code) DO NOTHING;
