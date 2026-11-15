-- name: OpenCase :one
-- A charge already under review returns no row, and the transaction goes on.
INSERT INTO review_cases (kind, provider, external_id, provider_key, payment_id, attempt_id, amount, currency,
                          ledger_reference, details, created_at)
VALUES (@kind, @provider, @external_id, sqlc.narg(provider_key), sqlc.narg(payment_id), sqlc.narg(attempt_id),
        @amount, @currency, sqlc.narg(ledger_reference), @details, @created_at)
ON CONFLICT (provider, external_id) DO NOTHING
RETURNING id;

-- name: HeldBetween :many
-- The money a case holds is a movement the gateway recorded, dated when it was held.
SELECT external_id, provider_key, attempt_id, amount, currency, created_at
FROM review_cases
WHERE provider = @provider AND ledger_reference IS NOT NULL
  AND created_at >= @from_at AND created_at < @to_at;
