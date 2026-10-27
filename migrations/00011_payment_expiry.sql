-- +goose Up
-- Section 4.6: the expiry job's queue. A pending payment is due when its quote
-- expires; the job pushes the time forward as a lease while it asks the provider, and
-- leaving pending takes the payment out.
ALTER TABLE payments ADD COLUMN expiry_check_at timestamptz;

UPDATE payments AS p SET expiry_check_at = q.expires_at
FROM quotes AS q
WHERE q.id = p.quote_id AND p.status = 'pending';

ALTER TABLE payments ADD CONSTRAINT payments_pending_is_queued
    CHECK ((status = 'pending') = (expiry_check_at IS NOT NULL));

CREATE INDEX payments_expiry_due ON payments (expiry_check_at) WHERE expiry_check_at IS NOT NULL;

-- +goose Down
DROP INDEX payments_expiry_due;
ALTER TABLE payments DROP CONSTRAINT payments_pending_is_queued;
ALTER TABLE payments DROP COLUMN expiry_check_at;
