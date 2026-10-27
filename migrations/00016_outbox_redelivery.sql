-- +goose Up
-- A manual redelivery (POST /v1/events/{id}/redeliver) opens a new retry window: the
-- 72 h of decision 23 count from window_started_at, and the backoff restarts from its
-- base at window_first_attempt. attempts keeps counting every claim, so it still fences
-- a claim from before the redelivery.
ALTER TABLE outbox_events
    ADD COLUMN window_started_at    timestamptz,
    ADD COLUMN window_first_attempt integer NOT NULL DEFAULT 1 CHECK (window_first_attempt >= 1);

UPDATE outbox_events SET window_started_at = created_at;

ALTER TABLE outbox_events
    ALTER COLUMN window_started_at SET NOT NULL,
    ADD CHECK (window_first_attempt <= attempts + 1);

-- +goose Down
ALTER TABLE outbox_events
    DROP COLUMN window_first_attempt,
    DROP COLUMN window_started_at;
