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
INSERT INTO outbox_events (id, merchant_id, type, subject_id, payload, created_at)
VALUES (@id, @merchant_id, @type, @subject_id, @payload, @created_at);
