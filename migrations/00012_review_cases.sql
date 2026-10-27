-- +goose Up
-- Section 4.4: money the provider confirmed that cannot confirm a payment still enters
-- the ledger, on the divergence account, and a case asks a person where it goes
-- (decision 8, in F7). One case per provider charge: the unique key is what makes a
-- charge seen twice, by the event and by the expiry job, post once.
CREATE TABLE review_cases (
    id               uuid PRIMARY KEY DEFAULT uuidv7(),
    kind             text NOT NULL
                     CHECK (kind IN ('late_payment', 'amount_mismatch', 'paid_after_rejection', 'second_charge')),
    provider         text NOT NULL CHECK (length(provider) BETWEEN 1 AND 50),
    external_id      text NOT NULL CHECK (length(external_id) BETWEEN 1 AND 100),
    payment_id       uuid REFERENCES payments (id),
    attempt_id       uuid REFERENCES payment_attempts (id),
    amount           bigint NOT NULL CHECK (amount > 0),
    currency         text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    -- The case is written before the posting it names, in the same transaction, so the
    -- check waits for the commit: by then the posting exists or nothing does.
    ledger_reference text NOT NULL REFERENCES ledger_transactions (reference) DEFERRABLE INITIALLY DEFERRED,
    details          text NOT NULL,
    status           text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    created_at       timestamptz NOT NULL,
    resolved_at      timestamptz,
    UNIQUE (provider, external_id),
    CHECK ((status = 'resolved') = (resolved_at IS NOT NULL))
);

CREATE INDEX review_cases_open ON review_cases (created_at) WHERE status = 'open';

-- +goose Down
DROP TABLE review_cases;
