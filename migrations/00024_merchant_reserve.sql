-- +goose Up
-- Decision 26: every settlement holds a share of what was available in the merchant's
-- reserve until reserve_until, and the first run of that cutoff or later gives it back
-- to the payable and marks it released. Held money can also cover a negative balance
-- (decision 27); the ledger tells how much of the reserve is left, these columns when
-- each part comes due.
ALTER TABLE settlements
    ADD COLUMN reserved            bigint NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    ADD COLUMN reserve_until       date,
    ADD COLUMN reserve_released_on date,
    ADD CONSTRAINT settlements_reserve_until CHECK ((reserved = 0) = (reserve_until IS NULL)),
    ADD CONSTRAINT settlements_reserve_released CHECK (
        reserve_released_on IS NULL OR reserve_released_on >= reserve_until);

CREATE INDEX settlements_reserve_held ON settlements (merchant_id, reserve_until)
WHERE reserve_until IS NOT NULL AND reserve_released_on IS NULL;

-- +goose Down
DROP INDEX settlements_reserve_held;
ALTER TABLE settlements
    DROP COLUMN reserve_released_on,
    DROP COLUMN reserve_until,
    DROP COLUMN reserved;
