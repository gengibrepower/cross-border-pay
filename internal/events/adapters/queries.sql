-- name: InsertEvent :one
-- A redelivery hits the unique key and returns no row.
INSERT INTO inbox_events (provider, external_id, type, payload, signature, next_attempt_at, received_at)
VALUES (@provider, @external_id, @type, @payload, @signature, @received_at, @received_at)
ON CONFLICT (provider, external_id) DO NOTHING
RETURNING id;

-- name: StoredPayload :one
SELECT payload FROM inbox_events WHERE provider = @provider AND external_id = @external_id;

-- name: ClaimDueEvent :one
-- The row lock lasts the whole transaction that applies the event, so the effect and
-- the event's new status commit together. SKIP LOCKED sends other workers elsewhere.
SELECT * FROM inbox_events
WHERE status = 'pending' AND next_attempt_at <= @now
ORDER BY next_attempt_at
LIMIT 1
FOR UPDATE SKIP LOCKED;

-- name: FinishEvent :execrows
UPDATE inbox_events
SET status = @status, subject_id = sqlc.narg(subject_id), last_error = sqlc.narg(last_error),
    next_attempt_at = NULL, processed_at = @processed_at
WHERE id = @id AND status = 'pending';

-- name: RetryEvent :execrows
-- Only while pending: another worker may have finished it since this one gave up.
UPDATE inbox_events
SET attempts = attempts + 1, next_attempt_at = @next_attempt_at, last_error = @last_error
WHERE id = @id AND status = 'pending';

-- name: GetEvent :one
SELECT * FROM inbox_events WHERE id = @id;

-- name: AppendOutboxEvent :exec
INSERT INTO outbox_events (id, merchant_id, type, subject_id, payload, created_at, next_attempt_at)
VALUES (@id, @merchant_id, @type, @subject_id, @payload, @created_at, @created_at);

-- name: ClaimDueDeliveries :many
-- Up to per_merchant events of each merchant with something due, oldest merchant
-- first, so one merchant's backlog cannot take the whole batch. The claim commits by
-- itself: the lease, not a row lock, is what keeps other workers away while the event
-- is posted. SKIP LOCKED only covers two claims running at the same instant.
WITH merchants_due AS (
    SELECT p.merchant_id, min(p.next_attempt_at) AS oldest
    FROM outbox_events p
    WHERE p.delivery_status = 'pending' AND p.next_attempt_at <= @now
    GROUP BY p.merchant_id
    ORDER BY oldest
    LIMIT @max_merchants
), claimed AS (
    SELECT e.id
    FROM merchants_due m
    CROSS JOIN LATERAL (
        SELECT d.id FROM outbox_events d
        WHERE d.merchant_id = m.merchant_id AND d.delivery_status = 'pending' AND d.next_attempt_at <= @now
        ORDER BY d.next_attempt_at
        LIMIT @per_merchant
        FOR UPDATE SKIP LOCKED
    ) e
    LIMIT @max_events
)
UPDATE outbox_events o
SET attempts = o.attempts + 1, next_attempt_at = @lease_until
FROM claimed
WHERE o.id = claimed.id
RETURNING o.id, o.merchant_id, o.type, o.subject_id, o.payload, o.created_at, o.attempts;

-- name: MarkDelivered :execrows
-- Not fenced by attempts: a 2xx is a fact even from a claim whose lease ran out.
UPDATE outbox_events
SET delivery_status = 'delivered', next_attempt_at = NULL, finished_at = @now,
    last_status_code = @status_code, last_error = NULL
WHERE id = @id AND delivery_status = 'pending';

-- name: RescheduleDelivery :execrows
UPDATE outbox_events
SET next_attempt_at = @next_attempt_at, last_status_code = sqlc.narg(status_code), last_error = @last_error
WHERE id = @id AND delivery_status = 'pending' AND attempts = @attempts;

-- name: FailDelivery :execrows
UPDATE outbox_events
SET delivery_status = 'failed', next_attempt_at = NULL, finished_at = @now,
    last_status_code = sqlc.narg(status_code), last_error = @last_error
WHERE id = @id AND delivery_status = 'pending' AND attempts = @attempts;

-- name: DeleteDeliveredEvents :execrows
-- One batch of events delivered before the cutoff. SKIP LOCKED passes over a row
-- another statement holds instead of waiting for it.
DELETE FROM outbox_events
WHERE id IN (
    SELECT old.id FROM outbox_events AS old
    WHERE old.delivery_status = 'delivered' AND old.finished_at <= @cutoff
    LIMIT @max_rows::bigint
    FOR UPDATE SKIP LOCKED
);
