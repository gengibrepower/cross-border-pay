-- +goose Up
CREATE TABLE merchants (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Only the SHA-256 of a key is stored (decision 33); the key itself is shown once.
CREATE TABLE api_keys (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES merchants (id),
    mode        text NOT NULL CHECK (mode IN ('test', 'live')),
    lookup      text NOT NULL UNIQUE,
    key_hash    bytea NOT NULL CHECK (length(key_hash) = 32),
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

CREATE INDEX api_keys_merchant_id ON api_keys (merchant_id);

-- +goose Down
DROP TABLE api_keys;
DROP TABLE merchants;
