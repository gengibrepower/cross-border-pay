-- +goose Up
-- Decisions 22 and 23: the delivery state of each outbox event. A worker claims a due
-- event by moving next_attempt_at past a lease and bumping attempts, commits, and only
-- then posts it, outside any transaction (section 4.3). A worker that dies mid-post
-- leaves the lease to run out, and the event is due again. attempts also fences what
-- a worker records: only the claim that is still current may reschedule or fail it.
ALTER TABLE outbox_events
    ADD COLUMN delivery_status  text NOT NULL DEFAULT 'pending'
        CHECK (delivery_status IN ('pending', 'delivered', 'failed')),
    ADD COLUMN attempts         integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    ADD COLUMN next_attempt_at  timestamptz,
    ADD COLUMN last_status_code integer CHECK (last_status_code BETWEEN 100 AND 999),
    ADD COLUMN last_error       text CHECK (length(last_error) <= 500),
    ADD COLUMN finished_at      timestamptz;

UPDATE outbox_events SET next_attempt_at = created_at;

ALTER TABLE outbox_events
    ADD CHECK ((delivery_status = 'pending') = (next_attempt_at IS NOT NULL)),
    ADD CHECK ((delivery_status = 'pending') = (finished_at IS NULL));

CREATE INDEX outbox_events_due ON outbox_events (merchant_id, next_attempt_at)
    WHERE delivery_status = 'pending';

-- +goose Down
DROP INDEX outbox_events_due;
ALTER TABLE outbox_events
    DROP COLUMN finished_at,
    DROP COLUMN last_error,
    DROP COLUMN last_status_code,
    DROP COLUMN next_attempt_at,
    DROP COLUMN attempts,
    DROP COLUMN delivery_status;
