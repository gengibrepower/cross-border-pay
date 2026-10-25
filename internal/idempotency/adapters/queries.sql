-- name: InsertKey :execrows
INSERT INTO idempotency_keys (merchant_id, key, request_hash, status, recovery_point, lock_token, locked_until, created_at)
VALUES (@merchant_id, @key, @request_hash, 'processing', 'started', @lock_token, @locked_until, @created_at)
ON CONFLICT (merchant_id, key) DO NOTHING;

-- name: LockKey :one
SELECT * FROM idempotency_keys
WHERE merchant_id = @merchant_id AND key = @key
FOR UPDATE;

-- name: RestartKey :exec
UPDATE idempotency_keys
SET request_hash = @request_hash, status = 'processing', recovery_point = 'started',
    resource_id = NULL, lock_token = @lock_token, locked_until = @locked_until,
    response_code = NULL, response_headers = NULL, response_body = NULL, created_at = @created_at
WHERE merchant_id = @merchant_id AND key = @key;

-- name: ResumeKey :exec
UPDATE idempotency_keys
SET lock_token = @lock_token, locked_until = @locked_until
WHERE merchant_id = @merchant_id AND key = @key;

-- The lock_token condition in the statements below is what makes a lost lease harmless:
-- only the current holder matches, and a completed key has no token at all.

-- name: CompleteKey :execrows
UPDATE idempotency_keys
SET status = 'completed', response_code = @response_code::bigint, response_headers = @response_headers,
    response_body = @response_body, lock_token = NULL, locked_until = NULL
WHERE merchant_id = @merchant_id AND key = @key AND lock_token = @lock_token;

-- name: ReleaseKey :execrows
DELETE FROM idempotency_keys
WHERE merchant_id = @merchant_id AND key = @key AND lock_token = @lock_token AND recovery_point = 'started';

-- name: EndLease :execrows
UPDATE idempotency_keys
SET locked_until = @now
WHERE merchant_id = @merchant_id AND key = @key AND lock_token = @lock_token;

-- name: SetRecoveryPoint :execrows
UPDATE idempotency_keys
SET recovery_point = @recovery_point, resource_id = @resource_id
WHERE merchant_id = @merchant_id AND key = @key AND lock_token = @lock_token;
