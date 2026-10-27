-- +goose Up
-- Section 4.4 and decision 19: the provider's webhook only stores the event here and
-- answers 200; a worker applies it. The unique external ID turns a redelivery into a
-- no-op, and payload with signature keep the exact bytes the provider signed, so the
-- proof can be checked again later (section 6).
CREATE TABLE inbox_events (
    id              uuid PRIMARY KEY DEFAULT uuidv7(),
    provider        text NOT NULL CHECK (length(provider) BETWEEN 1 AND 50),
    external_id     text NOT NULL CHECK (length(external_id) BETWEEN 1 AND 100),
    type            text NOT NULL CHECK (length(type) BETWEEN 1 AND 100),
    payload         bytea NOT NULL,
    signature       text NOT NULL,
    status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'processed', 'ignored', 'failed')),
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz,
    last_error      text,
    subject_id      text,
    received_at     timestamptz NOT NULL,
    processed_at    timestamptz,
    UNIQUE (provider, external_id),
    -- Pending is exactly the queue, and only a finished event has a finish time.
    CHECK ((status = 'pending') = (next_attempt_at IS NOT NULL)),
    CHECK ((status = 'pending') = (processed_at IS NULL))
);

CREATE INDEX inbox_events_due ON inbox_events (next_attempt_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE inbox_events;
