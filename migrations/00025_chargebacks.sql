-- +goose Up
-- F8, section 4.7: a cardholder's bank taking a payment's money back. The money is
-- gone when the chargeback opens, so its amounts are fixed then and posted at once; the
-- dispute after it only decides whether the bank gives them back.
CREATE TABLE chargebacks (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    payment_id     uuid NOT NULL REFERENCES payments (id),
    merchant_id    uuid NOT NULL REFERENCES merchants (id),
    provider       text NOT NULL,
    external_id    text NOT NULL,
    reason         text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 100),
    status         text NOT NULL CHECK (status IN ('needs_response', 'under_review', 'won', 'lost')),
    -- What the bank took, in the payer's currency, and the merchant's and the spread's
    -- shares of it, in the merchant's.
    payer_amount   bigint NOT NULL CHECK (payer_amount > 0),
    payer_currency text NOT NULL CHECK (payer_currency ~ '^[A-Z]{3}$'),
    amount         bigint NOT NULL CHECK (amount >= 0),
    spread         bigint NOT NULL,
    currency       text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    evidence       text CHECK (char_length(evidence) BETWEEN 1 AND 2000),
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    closed_at      timestamptz,
    -- A provider's event is applied once, however often it is delivered.
    UNIQUE (provider, external_id),
    CONSTRAINT chargebacks_closed CHECK ((status IN ('won', 'lost')) = (closed_at IS NOT NULL))
);

CREATE INDEX chargebacks_payment ON chargebacks (payment_id);

-- +goose Down
DROP TABLE chargebacks;
