-- name: InsertQuote :one
INSERT INTO quotes (
    base_currency, quote_currency, base_rate, spread_bps, fee_bps,
    amount, payer_amount, converted, spread, rounding, fee, merchant_net,
    created_at, expires_at
) VALUES (
    @base_currency, @quote_currency, @base_rate, @spread_bps, @fee_bps,
    @amount, @payer_amount, @converted, @spread, @rounding, @fee, @merchant_net,
    @created_at, @expires_at
)
RETURNING id;
