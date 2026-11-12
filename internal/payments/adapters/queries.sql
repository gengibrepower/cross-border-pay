-- name: InsertPayment :one
INSERT INTO payments (merchant_id, quote_id, status, method, amount, currency, reference, created_at, updated_at, expiry_check_at)
VALUES (@merchant_id, @quote_id, @status, @method, @amount, @currency, @reference, @created_at, @created_at, @expiry_check_at)
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

-- name: LockPaymentOfAttempt :one
-- Section 4.4: events of one payment are applied one at a time. Every path that changes
-- an attempt takes this lock first, then the attempt's, then the ledger's accounts, so
-- no two paths wait on each other in opposite orders. NO KEY UPDATE is the lock an
-- update takes; unlike FOR UPDATE, it does not wait for the KEY SHARE that a foreign
-- key check holds on the payment.
SELECT p.id AS payment_id, p.merchant_id, p.status, p.version, p.amount, p.currency, p.quote_id,
    q.payer_amount, q.quote_currency
FROM payment_attempts AS a
JOIN payments AS p ON p.id = a.payment_id
JOIN quotes AS q ON q.id = p.quote_id
WHERE a.id = @id
FOR NO KEY UPDATE OF p;

-- name: UpdatePaymentStatus :one
-- Every status after pending is final for expiry, so the payment leaves its queue.
UPDATE payments SET status = @status, version = version + 1, updated_at = @updated_at, expiry_check_at = NULL
WHERE id = @id
RETURNING version;

-- name: ClaimDueExpiries :many
-- The expiry job's claim, like ClaimDueAttempts: SKIP LOCKED during this short
-- transaction, then the pushed expiry_check_at as a lease while the provider is asked.
-- NO KEY UPDATE, as everywhere a payment is locked to change it.
UPDATE payments SET expiry_check_at = @lease_until
WHERE id IN (
    SELECT due.id FROM payments AS due
    WHERE due.expiry_check_at <= @now
    ORDER BY due.expiry_check_at
    LIMIT @max_rows::bigint
    FOR NO KEY UPDATE SKIP LOCKED
)
RETURNING id;

-- name: LockMerchantPayment :one
SELECT id, merchant_id, status, amount, currency FROM payments
WHERE merchant_id = @merchant_id AND id = @id
FOR NO KEY UPDATE;

-- name: LockPayment :one
SELECT p.id, p.merchant_id, p.status, p.amount, p.currency, q.expires_at
FROM payments AS p
JOIN quotes AS q ON q.id = p.quote_id
WHERE p.id = @id
FOR NO KEY UPDATE OF p;

-- name: NewEventID :one
-- Every ID is a UUIDv7 the database issues; an event's is needed before its row
-- exists, because the payload carries it.
SELECT uuidv7()::uuid AS id;

-- name: LockRefundablePayment :one
-- The refund's lock: every refund of a payment, and every confirmation, waits for it,
-- so two refunds never sum what counts at the same time (invariant 8).
SELECT id, merchant_id, status, version, amount, currency, quote_id FROM payments
WHERE merchant_id = @merchant_id AND id = @id
FOR NO KEY UPDATE;

-- name: CountedRefunds :one
-- What a payment's refunds take, every one but the rejected: the running total a new
-- refund is priced against.
SELECT coalesce(sum(amount), 0)::bigint AS amount, coalesce(sum(payer_amount), 0)::bigint AS payer_amount,
    coalesce(sum(spread), 0)::bigint AS spread
FROM refunds WHERE payment_id = @payment_id AND status <> 'rejected';

-- name: SucceededRefunds :one
SELECT coalesce(sum(amount), 0)::bigint FROM refunds WHERE payment_id = @payment_id AND status = 'succeeded';

-- name: InsertRefund :one
INSERT INTO refunds (payment_id, provider, status, amount, currency, payer_amount, payer_currency, spread, created_at, updated_at, next_check_at)
VALUES (@payment_id, @provider, 'created', @amount, @currency, @payer_amount, @payer_currency, @spread, @created_at, @created_at, @next_check_at)
RETURNING *;

-- name: GetMerchantRefund :one
SELECT r.* FROM refunds AS r
JOIN payments AS p ON p.id = r.payment_id
WHERE r.id = @id AND p.merchant_id = @merchant_id;

-- name: LockPaymentOfRefund :one
-- The same lock order as attempts: payment, then the refund, then ledger accounts.
SELECT p.id, p.merchant_id, p.status, p.version, p.amount, p.currency, p.quote_id
FROM refunds AS r
JOIN payments AS p ON p.id = r.payment_id
WHERE r.id = @id
FOR NO KEY UPDATE OF p;

-- name: LockRefund :one
SELECT * FROM refunds WHERE id = @id FOR UPDATE;

-- name: UpdateRefund :exec
UPDATE refunds
SET status = @status, external_id = sqlc.narg(external_id), reason = sqlc.narg(reason), updated_at = @updated_at,
    checks = @checks, next_check_at = sqlc.narg(next_check_at)
WHERE id = @id;

-- name: ClaimDueRefunds :many
-- ClaimDueAttempts for refunds: SKIP LOCKED during this short transaction, then the
-- lease in next_check_at while the provider is called.
UPDATE refunds SET next_check_at = @lease_until
WHERE id IN (
    SELECT due.id FROM refunds AS due
    WHERE due.next_check_at <= @now
    ORDER BY due.next_check_at
    LIMIT @max_rows::bigint
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: RescheduleRefund :execrows
UPDATE refunds SET checks = checks + 1, next_check_at = @next_check_at
WHERE id = @id AND status = 'unknown';

-- name: EscalateRefund :execrows
UPDATE refunds SET next_check_at = NULL, escalated_at = @escalated_at
WHERE id = @id AND status IN ('sent', 'unknown');

-- name: SucceededAttempt :one
-- At most one: a payment has one live attempt at a time.
SELECT * FROM payment_attempts WHERE payment_id = @payment_id AND status = 'succeeded';

-- name: SetPaidAt :exec
UPDATE payments SET paid_at = @paid_at WHERE id = @id;

-- name: SettlementCandidates :many
-- Merchants with confirmed money and no settlement for the cutoff yet, after the last
-- one seen, so a round pages through all of them.
SELECT DISTINCT p.merchant_id FROM payments AS p
WHERE p.paid_at IS NOT NULL AND p.merchant_id > @after
    AND NOT EXISTS (SELECT 1 FROM settlements AS s WHERE s.merchant_id = p.merchant_id AND s.cutoff_date = @cutoff_date)
ORDER BY p.merchant_id
LIMIT @max_rows::bigint;

-- name: LockSettleablePayments :many
-- The merchant's confirmed payments available by the cutoff and in no settlement yet,
-- each method by its own availability (decision 25). Locked first, before the ledger,
-- in the order every path takes: payment, then accounts.
SELECT p.id FROM payments AS p
WHERE p.merchant_id = @merchant_id AND p.paid_at IS NOT NULL AND p.settlement_id IS NULL
    AND p.paid_at < CASE p.method
        WHEN 'pix' THEN @pix_before::timestamptz
        WHEN 'boleto' THEN @boleto_before::timestamptz
        ELSE @card_before::timestamptz END
ORDER BY p.id
FOR NO KEY UPDATE;

-- name: PendingNet :one
-- What the merchant's payable holds for payments not available yet: each one's net,
-- less its refunds that still count. Every refund takes its amount from the payable
-- when created and only a rejection gives it back (migration 00017), so this is
-- exactly their share of the payable's balance.
SELECT coalesce(sum(q.merchant_net - r.refunded), 0)::bigint
FROM payments AS p
JOIN quotes AS q ON q.id = p.quote_id
CROSS JOIN LATERAL (
    SELECT coalesce(sum(amount), 0)::bigint AS refunded FROM refunds
    WHERE payment_id = p.id AND status <> 'rejected'
) AS r
WHERE p.merchant_id = @merchant_id AND p.paid_at IS NOT NULL
    AND p.paid_at >= CASE p.method
        WHEN 'pix' THEN @pix_before::timestamptz
        WHEN 'boleto' THEN @boleto_before::timestamptz
        ELSE @card_before::timestamptz END;

-- name: InsertSettlement :one
-- No row when the cutoff is already settled: a concurrent run got there first.
INSERT INTO settlements (merchant_id, cutoff_date, amount, currency, created_at)
VALUES (@merchant_id, @cutoff_date, @amount, @currency, @created_at)
ON CONFLICT (merchant_id, cutoff_date) DO NOTHING
RETURNING *;

-- name: MarkSettled :exec
UPDATE payments SET settlement_id = @settlement_id WHERE id = ANY(@ids::uuid[]);

-- name: ListSettlements :many
-- Keyset pagination on settlements_merchant_id_cutoff_date_key: newest cutoff first,
-- each page starts below the last cutoff of the previous one, unique per merchant.
SELECT * FROM settlements
WHERE merchant_id = @merchant_id
  AND (sqlc.narg(before)::date IS NULL OR cutoff_date < sqlc.narg(before))
ORDER BY cutoff_date DESC
LIMIT @max_rows::bigint;

-- name: SettlementPayments :many
-- On payments_settlement (migration 00020).
SELECT settlement_id, id FROM payments
WHERE settlement_id = ANY(@settlement_ids::uuid[])
ORDER BY settlement_id, id;
