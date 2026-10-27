-- +goose Up
-- Section 4.5: an event for a merchant is written in the same transaction as the fact
-- it reports. payload holds the exact bytes every delivery will send and sign, so
-- redeliveries are identical and the merchant can drop duplicates by the event's id.
-- Delivery state arrives with the delivery worker in F6 (decisions 22 to 24).
CREATE TABLE outbox_events (
    id          uuid PRIMARY KEY,
    merchant_id uuid NOT NULL REFERENCES merchants (id),
    type        text NOT NULL CHECK (length(type) BETWEEN 1 AND 100),
    subject_id  text NOT NULL CHECK (length(subject_id) BETWEEN 1 AND 100),
    payload     bytea NOT NULL,
    created_at  timestamptz NOT NULL
);

CREATE INDEX outbox_events_merchant_id ON outbox_events (merchant_id, id);

-- Section 4.5: grows with every transition, so a merchant can ignore an older event
-- that arrives after a newer one.
ALTER TABLE payments ADD COLUMN version integer NOT NULL DEFAULT 1 CHECK (version >= 1);

-- +goose Down
ALTER TABLE payments DROP COLUMN version;
DROP TABLE outbox_events;
