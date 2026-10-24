-- +goose Up
-- Owned by fx. Every computed amount is stored, not just the inputs, so a quote stays
-- what the payer was shown even if the pricing formula changes later.
CREATE TABLE quotes (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    base_currency  text NOT NULL,
    quote_currency text NOT NULL,
    base_rate      bigint NOT NULL CHECK (base_rate > 0),
    spread_bps     bigint NOT NULL CHECK (spread_bps BETWEEN 0 AND 10000),
    fee_bps        bigint NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
    amount         bigint NOT NULL CHECK (amount > 0),
    payer_amount   bigint NOT NULL,
    converted      bigint NOT NULL,
    spread         bigint NOT NULL,
    rounding       bigint NOT NULL,
    fee            bigint NOT NULL,
    merchant_net   bigint NOT NULL,
    created_at     timestamptz NOT NULL,
    expires_at     timestamptz NOT NULL,
    CHECK (base_currency <> quote_currency),
    CHECK (expires_at > created_at),
    -- The identities from section 3.3 that make the USD legs sum to zero.
    CHECK (converted = amount + spread + rounding),
    CHECK (merchant_net + fee = amount)
);

CREATE TABLE payments (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES merchants (id),
    quote_id    uuid NOT NULL REFERENCES quotes (id),
    status      text NOT NULL CHECK (status IN (
        'pending', 'paid', 'partially_refunded', 'refunded', 'charged_back', 'expired', 'canceled')),
    method      text NOT NULL CHECK (method IN ('pix', 'boleto', 'card')),
    amount      bigint NOT NULL CHECK (amount > 0),
    currency    text NOT NULL,
    reference   text NOT NULL CHECK (length(reference) BETWEEN 1 AND 100),
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);

CREATE INDEX payments_merchant_id_id ON payments (merchant_id, id);

-- +goose Down
DROP TABLE payments;
DROP TABLE quotes;
