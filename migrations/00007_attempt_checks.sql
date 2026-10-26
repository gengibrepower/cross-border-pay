-- +goose Up
-- The queue of attempts to look up at the provider (decision 17). next_check_at is
-- both the schedule and the claim: a worker pushes it a lease ahead before calling the
-- provider, so no transaction stays open across the network.
ALTER TABLE payment_attempts
    ADD COLUMN checks        integer NOT NULL DEFAULT 0 CHECK (checks >= 0),
    ADD COLUMN next_check_at timestamptz,
    ADD COLUMN escalated_at  timestamptz,
    ADD CHECK (next_check_at IS NULL OR status IN ('sent', 'unknown')),
    ADD CHECK (escalated_at IS NULL OR next_check_at IS NULL);

CREATE INDEX payment_attempts_due ON payment_attempts (next_check_at)
    WHERE next_check_at IS NOT NULL;

-- +goose Down
DROP INDEX payment_attempts_due;
ALTER TABLE payment_attempts
    DROP COLUMN escalated_at,
    DROP COLUMN next_check_at,
    DROP COLUMN checks;
