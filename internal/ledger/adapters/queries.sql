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
