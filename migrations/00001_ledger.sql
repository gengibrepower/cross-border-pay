-- +goose Up
CREATE TABLE ledger_accounts (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    code         text UNIQUE,
    currency     text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_rule text NOT NULL CHECK (balance_rule IN ('may_go_negative', 'must_stay_non_negative')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Target of the composite foreign key that pins each entry to its account's currency.
    UNIQUE (id, currency)
);

CREATE TABLE ledger_transactions (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    reference  text UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ledger_entries (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    transaction_id uuid NOT NULL REFERENCES ledger_transactions (id),
    account_id     uuid NOT NULL,
    currency       text NOT NULL,
    amount         bigint NOT NULL CHECK (amount <> 0),
    FOREIGN KEY (account_id, currency) REFERENCES ledger_accounts (id, currency)
);

CREATE INDEX ledger_entries_account_id ON ledger_entries (account_id);
CREATE INDEX ledger_entries_transaction_id ON ledger_entries (transaction_id);

-- Invariant 2: entries and transactions are never changed; corrections are new
-- transactions.
-- +goose StatementBegin
CREATE FUNCTION ledger_forbid_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger: % on % is forbidden, post a correcting transaction', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_immutable
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_change();
CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_change();
CREATE TRIGGER ledger_transactions_immutable
    BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_change();
CREATE TRIGGER ledger_transactions_no_truncate
    BEFORE TRUNCATE ON ledger_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_change();

-- +goose Down
DROP TABLE ledger_entries;
DROP TABLE ledger_transactions;
DROP TABLE ledger_accounts;
DROP FUNCTION ledger_forbid_change();
