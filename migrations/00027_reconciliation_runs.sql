-- +goose Up
-- Decision 6 of F10: one reconciliation per provider and day. A worker claims the day
-- with a lease; a run that failed or died is taken again once the lease ends.
CREATE TABLE reconciliation_runs (
    provider    text NOT NULL CHECK (length(provider) BETWEEN 1 AND 50),
    day         date NOT NULL,
    lease_until timestamptz NOT NULL,
    finished_at timestamptz,
    divergences integer NOT NULL DEFAULT 0 CHECK (divergences >= 0),
    settled     integer NOT NULL DEFAULT 0 CHECK (settled >= 0),
    opened      integer NOT NULL DEFAULT 0 CHECK (opened >= 0),
    PRIMARY KEY (provider, day)
);

-- +goose Down
DROP TABLE reconciliation_runs;
