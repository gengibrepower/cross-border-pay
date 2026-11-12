-- +goose Up
-- F7: the daily settlement (section 4.7), one per merchant and cutoff date in local
-- time (decision 20). A settlement pays out what is available; the payments that
-- became available for it point at it, so each is listed in exactly one.
CREATE TABLE settlements (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    merchant_id uuid NOT NULL REFERENCES merchants (id),
    cutoff_date date NOT NULL,
    amount      bigint NOT NULL CHECK (amount > 0),
    currency    text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    created_at  timestamptz NOT NULL,
    -- Section 4.2: running the cutoff twice pays once.
    UNIQUE (merchant_id, cutoff_date)
);

-- paid_at is when the confirmation was posted: from then on the money is the
-- merchant's, pending until the method's availability (decision 25) has passed.
ALTER TABLE payments
    ADD COLUMN paid_at       timestamptz,
    ADD COLUMN settlement_id uuid REFERENCES settlements (id),
    ADD CHECK (settlement_id IS NULL OR paid_at IS NOT NULL);

-- Payments confirmed before the column existed: their last change is no earlier than
-- the confirmation, so they become available no sooner than they should.
UPDATE payments SET paid_at = updated_at
WHERE status IN ('paid', 'partially_refunded', 'refunded', 'charged_back');

CREATE INDEX payments_paid ON payments (merchant_id, paid_at) WHERE paid_at IS NOT NULL;

-- +goose Down
DROP INDEX payments_paid;
ALTER TABLE payments
    DROP COLUMN settlement_id,
    DROP COLUMN paid_at;
DROP TABLE settlements;
