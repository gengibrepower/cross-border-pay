-- +goose Up
-- Decision 6: a payment has attempts, one per try at a provider. The row is written as
-- 'created' before the provider is called (section 4.3), so a crash mid-call still
-- leaves a trace to resolve.
CREATE TABLE payment_attempts (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    payment_id  uuid NOT NULL REFERENCES payments (id),
    provider    text NOT NULL CHECK (length(provider) BETWEEN 1 AND 50),
    status      text NOT NULL CHECK (status IN (
        'created', 'sent', 'unknown', 'accepted', 'rejected', 'succeeded')),
    external_id text CHECK (length(external_id) BETWEEN 1 AND 100),
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    CHECK (updated_at >= created_at),
    UNIQUE (provider, external_id)
);

-- At most one live attempt per payment: two concurrent requests cannot both create a
-- QR code. Only a rejection frees the payment for a new attempt.
CREATE UNIQUE INDEX payment_attempts_one_live ON payment_attempts (payment_id)
    WHERE status <> 'rejected';

-- +goose Down
DROP TABLE payment_attempts;
