-- +goose Up
-- Section 4.8: the daily reconciliation opens cases too. Most post nothing, since only
-- a person knows which side is right; money the gateway cannot tie to any attempt
-- still enters the ledger, as in 4.4, and keeps the key the provider named so the next
-- reconciliation finds it.
ALTER TABLE review_cases
    DROP CONSTRAINT review_cases_kind_check,
    ADD CONSTRAINT review_cases_kind_check CHECK (kind IN (
        'late_payment', 'amount_mismatch', 'paid_after_rejection', 'second_charge',
        'missing_in_system', 'missing_at_provider', 'amount_differs')),
    ALTER COLUMN ledger_reference DROP NOT NULL,
    ADD CONSTRAINT review_cases_posted CHECK (
        ledger_reference IS NOT NULL OR kind IN ('missing_in_system', 'missing_at_provider', 'amount_differs')),
    ADD COLUMN provider_key text CHECK (length(provider_key) BETWEEN 1 AND 100);

-- +goose Down
ALTER TABLE review_cases
    DROP COLUMN provider_key,
    DROP CONSTRAINT review_cases_posted,
    DROP CONSTRAINT review_cases_kind_check;
DELETE FROM review_cases WHERE ledger_reference IS NULL
    OR kind IN ('missing_in_system', 'missing_at_provider', 'amount_differs');
ALTER TABLE review_cases
    ALTER COLUMN ledger_reference SET NOT NULL,
    ADD CONSTRAINT review_cases_kind_check
        CHECK (kind IN ('late_payment', 'amount_mismatch', 'paid_after_rejection', 'second_charge'));
