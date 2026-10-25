-- name: InsertPayment :one
INSERT INTO payments (merchant_id, quote_id, status, method, amount, currency, reference, created_at, updated_at)
VALUES (@merchant_id, @quote_id, @status, @method, @amount, @currency, @reference, @created_at, @created_at)
RETURNING id;

-- name: GetPayment :one
SELECT * FROM payments WHERE merchant_id = @merchant_id AND id = @id;

-- name: ListPayments :many
-- Keyset pagination on payments_merchant_id_id: newest first, each page starts below
-- the last ID of the previous one.
SELECT * FROM payments
WHERE merchant_id = @merchant_id
  AND (sqlc.narg(before)::uuid IS NULL OR id < sqlc.narg(before))
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
ORDER BY id DESC
LIMIT @max_rows::bigint;
