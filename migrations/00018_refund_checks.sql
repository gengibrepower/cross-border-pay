-- +goose Up
-- The queue of refunds to finish (decision 17), as 00007 is for attempts: a created
-- refund whose request died is sent, a sent one that went stale and an unknown one are
-- looked up at the provider. next_check_at is both the schedule and the claim.
ALTER TABLE refunds
    ADD COLUMN checks        integer NOT NULL DEFAULT 0 CHECK (checks >= 0),
    ADD COLUMN next_check_at timestamptz,
    ADD COLUMN escalated_at  timestamptz,
    ADD CHECK (next_check_at IS NULL OR status IN ('created', 'sent', 'unknown')),
    ADD CHECK (escalated_at IS NULL OR next_check_at IS NULL);

-- Refunds left waiting before the queue existed are due at once.
UPDATE refunds SET next_check_at = now() WHERE status IN ('created', 'sent', 'unknown');

CREATE INDEX refunds_due ON refunds (next_check_at)
    WHERE next_check_at IS NOT NULL;

-- +goose Down
DROP INDEX refunds_due;
ALTER TABLE refunds
    DROP COLUMN escalated_at,
    DROP COLUMN next_check_at,
    DROP COLUMN checks;
