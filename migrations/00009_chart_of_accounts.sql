-- +goose Up
-- Section 3.3: the gateway's own accounts, one row per currency they hold. Each
-- merchant's payable is created by its first confirmation, not here. Only accounts that
-- only ever take money in must stay non-negative; the merchant payable may go negative
-- by decision 27.
INSERT INTO ledger_accounts (code, currency, balance_rule) VALUES
    ('provider_receivables:BRL', 'BRL', 'must_stay_non_negative'),
    ('fx:BRL', 'BRL', 'may_go_negative'),
    ('fx:USD', 'USD', 'may_go_negative'),
    ('fee_revenue:USD', 'USD', 'must_stay_non_negative'),
    ('spread_revenue:USD', 'USD', 'must_stay_non_negative'),
    ('fx_result:USD', 'USD', 'may_go_negative'),
    ('rounding:BRL', 'BRL', 'may_go_negative'),
    ('rounding:USD', 'USD', 'may_go_negative'),
    ('divergence:BRL', 'BRL', 'may_go_negative'),
    ('settlements_paid:USD', 'USD', 'may_go_negative');

-- +goose Down
DELETE FROM ledger_accounts WHERE code IN (
    'provider_receivables:BRL', 'fx:BRL', 'fx:USD', 'fee_revenue:USD', 'spread_revenue:USD',
    'fx_result:USD', 'rounding:BRL', 'rounding:USD', 'divergence:BRL', 'settlements_paid:USD');
