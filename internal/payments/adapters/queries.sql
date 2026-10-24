-- name: InsertPayment :one
INSERT INTO payments (merchant_id, quote_id, status, method, amount, currency, reference, created_at, updated_at)
VALUES (@merchant_id, @quote_id, @status, @method, @amount, @currency, @reference, @created_at, @created_at)
RETURNING id;
