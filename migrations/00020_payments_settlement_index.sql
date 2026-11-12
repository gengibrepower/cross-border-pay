-- +goose Up
-- GET /v1/settlements lists the payments of each settlement in the page. Payments of
-- no settlement yet are left out of the index: they are the ones that change.
CREATE INDEX payments_settlement ON payments (settlement_id) WHERE settlement_id IS NOT NULL;

-- +goose Down
DROP INDEX payments_settlement;
