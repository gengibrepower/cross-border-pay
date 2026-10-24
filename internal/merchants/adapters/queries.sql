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
