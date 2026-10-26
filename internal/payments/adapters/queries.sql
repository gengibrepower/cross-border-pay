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

-- name: InsertAttempt :one
INSERT INTO payment_attempts (payment_id, provider, status, created_at, updated_at)
VALUES (@payment_id, @provider, 'created', @created_at, @created_at)
RETURNING *;

-- name: GetAttempt :one
SELECT * FROM payment_attempts WHERE id = @id;

-- name: LockAttempt :one
SELECT * FROM payment_attempts WHERE id = @id FOR UPDATE;

-- name: UpdateAttempt :exec
UPDATE payment_attempts
SET status = @status, external_id = sqlc.narg(external_id), br_code = sqlc.narg(br_code), updated_at = @updated_at,
    checks = @checks, next_check_at = sqlc.narg(next_check_at)
WHERE id = @id;

-- name: LatestAttempts :many
SELECT DISTINCT ON (payment_id) * FROM payment_attempts
WHERE payment_id = ANY(@payment_ids::uuid[])
ORDER BY payment_id, id DESC;

-- name: ClaimDueAttempts :many
-- SKIP LOCKED keeps two workers off the same rows during this short transaction; the
-- lease written into next_check_at keeps them off after it commits, while the provider
-- is being called.
UPDATE payment_attempts SET next_check_at = @lease_until
WHERE id IN (
    SELECT due.id FROM payment_attempts AS due
    WHERE due.next_check_at <= @now
    ORDER BY due.next_check_at
    LIMIT @max_rows::bigint
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: RescheduleAttempt :execrows
UPDATE payment_attempts SET checks = checks + 1, next_check_at = @next_check_at
WHERE id = @id AND status = 'unknown';

-- name: EscalateAttempt :execrows
UPDATE payment_attempts SET next_check_at = NULL, escalated_at = @escalated_at
WHERE id = @id AND status IN ('sent', 'unknown');
