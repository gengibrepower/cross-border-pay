-- +goose Up
-- Decision 24: a destination that only fails is turned off. failing_since is the first
-- failure of a run with no success since; a failure 72 h after it disables the
-- endpoint, and any change of the endpoint by the merchant turns it back on.
ALTER TABLE webhook_endpoints
    ADD COLUMN failing_since   timestamptz,
    ADD COLUMN disabled_reason text CHECK (length(disabled_reason) BETWEEN 1 AND 500),
    ADD CHECK ((disabled_at IS NULL) = (disabled_reason IS NULL)),
    ADD CHECK (disabled_at IS NULL OR failing_since IS NOT NULL);

-- The cleanup of delivered events (section 4.5) walks them by age.
CREATE INDEX outbox_events_delivered ON outbox_events (finished_at)
    WHERE delivery_status = 'delivered';

-- +goose Down
DROP INDEX outbox_events_delivered;
ALTER TABLE webhook_endpoints
    DROP COLUMN disabled_reason,
    DROP COLUMN failing_since;
