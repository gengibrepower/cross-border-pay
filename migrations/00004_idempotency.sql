-- +goose Up
-- Section 4.2. The INSERT of a new key is the lock: the primary key lets only one of
-- two concurrent requests in. locked_until is a lease, not a row lock, so no
-- transaction stays open while the request runs.
CREATE TABLE idempotency_keys (
    merchant_id      uuid NOT NULL REFERENCES merchants (id),
    key              text NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    request_hash     bytea NOT NULL CHECK (length(request_hash) = 32),
    status           text NOT NULL CHECK (status IN ('processing', 'completed')),
    -- Opaque to this table: each endpoint names its own phases after 'started'.
    recovery_point   text NOT NULL,
    resource_id      uuid,
    lock_token       bytea CHECK (length(lock_token) = 16),
    locked_until     timestamptz,
    response_code    integer CHECK (response_code BETWEEN 100 AND 599),
    response_headers jsonb,
    response_body    bytea,
    created_at       timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, key),
    CHECK ((status = 'processing') = (lock_token IS NOT NULL AND locked_until IS NOT NULL)),
    CHECK ((status = 'completed') = (response_code IS NOT NULL AND response_headers IS NOT NULL AND response_body IS NOT NULL))
);

-- The cleanup job deletes by age (decision 14).
CREATE INDEX idempotency_keys_created_at ON idempotency_keys (created_at);

-- +goose Down
DROP TABLE idempotency_keys;
