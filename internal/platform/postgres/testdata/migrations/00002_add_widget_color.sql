-- +goose Up
ALTER TABLE widgets ADD COLUMN color text;

-- +goose Down
ALTER TABLE widgets DROP COLUMN color;
