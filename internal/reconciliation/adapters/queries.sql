-- name: OpenCase :one
-- A charge already under review returns no row, and the transaction goes on.
INSERT INTO review_cases (kind, provider, external_id, payment_id, attempt_id, amount, currency,
                          ledger_reference, details, created_at)
VALUES (@kind, @provider, @external_id, sqlc.narg(payment_id), sqlc.narg(attempt_id), @amount, @currency,
        @ledger_reference, @details, @created_at)
ON CONFLICT (provider, external_id) DO NOTHING
RETURNING id;
