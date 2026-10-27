-- name: CreateMerchant :one
INSERT INTO merchants (name)
VALUES (@name)
RETURNING id;

-- name: CreateAPIKey :exec
INSERT INTO api_keys (merchant_id, mode, lookup, key_hash)
VALUES (@merchant_id, @mode, @lookup, @key_hash);

-- name: FindActiveAPIKey :one
SELECT merchant_id, mode, key_hash
FROM api_keys
WHERE lookup = @lookup AND revoked_at IS NULL;

-- name: RevokeAPIKey :execrows
UPDATE api_keys
SET revoked_at = now()
WHERE lookup = @lookup AND revoked_at IS NULL;

-- name: LockMerchant :one
-- Serializes changes to one merchant's webhook endpoint, including its first
-- registration, when there is no endpoint row to lock yet. NO KEY UPDATE does not wait
-- for the KEY SHARE that inserts referencing the merchant take.
SELECT id FROM merchants WHERE id = @id FOR NO KEY UPDATE;

-- name: GetWebhookEndpoint :one
SELECT merchant_id, url, secret_version, previous_secret_version, rotated_at, created_at, updated_at
FROM webhook_endpoints
WHERE merchant_id = @merchant_id;

-- name: SaveWebhookEndpoint :exec
INSERT INTO webhook_endpoints (merchant_id, url, secret_version, previous_secret_version, rotated_at, created_at, updated_at)
VALUES (@merchant_id, @url, @secret_version, sqlc.narg(previous_secret_version), sqlc.narg(rotated_at), @created_at, @updated_at)
ON CONFLICT (merchant_id) DO UPDATE
SET url = excluded.url,
    secret_version = excluded.secret_version,
    previous_secret_version = excluded.previous_secret_version,
    rotated_at = excluded.rotated_at,
    updated_at = excluded.updated_at;
