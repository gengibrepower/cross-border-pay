-- +goose Up
-- One endpoint per merchant (PUT /v1/webhook-endpoint). No secret is stored: each one
-- is derived from a key kept outside the database and the version below, so a dump of
-- the database cannot forge a webhook (section 6). Rotating bumps the version; the
-- previous one is kept so deliveries can be signed with both while merchants switch.
CREATE TABLE webhook_endpoints (
    merchant_id             uuid PRIMARY KEY REFERENCES merchants (id),
    url                     text NOT NULL CHECK (length(url) BETWEEN 1 AND 2048),
    secret_version          integer NOT NULL CHECK (secret_version >= 1),
    previous_secret_version integer CHECK (previous_secret_version = secret_version - 1),
    rotated_at              timestamptz,
    -- Decision 24: a destination that only fails is turned off (F6, delivery).
    disabled_at             timestamptz,
    created_at              timestamptz NOT NULL,
    updated_at              timestamptz NOT NULL,
    CHECK ((previous_secret_version IS NULL) = (rotated_at IS NULL))
);

-- +goose Down
DROP TABLE webhook_endpoints;
