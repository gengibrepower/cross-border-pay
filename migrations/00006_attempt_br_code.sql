-- +goose Up
-- The BR Code is stored so that GET returns the same code the create returned, and a
-- payer who scanned it is never shown another one.
ALTER TABLE payment_attempts
    ADD COLUMN br_code text CHECK (length(br_code) BETWEEN 1 AND 512);

-- Reading a payment loads its latest attempt.
CREATE INDEX payment_attempts_payment_id_id ON payment_attempts (payment_id, id);

-- +goose Down
DROP INDEX payment_attempts_payment_id_id;
ALTER TABLE payment_attempts DROP COLUMN br_code;
