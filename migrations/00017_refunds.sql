-- +goose Up
-- F7: a payment's refunds, one row per refund at its provider. Like an attempt, the row
-- is written as 'created' before the provider is called (section 4.3), and it keeps the
-- amounts priced when it was created (decision 13), so a later change of formula does
-- not change what a refund moved.
CREATE TABLE refunds (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    payment_id     uuid NOT NULL REFERENCES payments (id),
    provider       text NOT NULL CHECK (length(provider) BETWEEN 1 AND 50),
    status         text NOT NULL CHECK (status IN ('created', 'sent', 'unknown', 'succeeded', 'rejected')),
    amount         bigint NOT NULL CHECK (amount > 0),
    currency       text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    payer_amount   bigint NOT NULL CHECK (payer_amount > 0),
    payer_currency text NOT NULL CHECK (payer_currency ~ '^[A-Z]{3}$'),
    -- Can be negative by a cent: the share of the spread is cumulative.
    spread         bigint NOT NULL,
    external_id    text CHECK (length(external_id) BETWEEN 1 AND 100),
    reason         text CHECK (length(reason) <= 500),
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    CHECK (updated_at >= created_at),
    UNIQUE (provider, external_id)
);

CREATE INDEX refunds_payment_id ON refunds (payment_id);

-- Holds what refunds took from merchants until the provider answers. It never goes
-- negative: a completion or a release only takes out what a hold put in.
INSERT INTO ledger_accounts (code, currency, balance_rule) VALUES
    ('refunds_in_flight:USD', 'USD', 'must_stay_non_negative');

-- +goose Down
DELETE FROM ledger_accounts WHERE code = 'refunds_in_flight:USD';
DROP TABLE refunds;
