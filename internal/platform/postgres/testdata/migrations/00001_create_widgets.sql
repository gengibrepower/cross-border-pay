-- +goose Up
CREATE TABLE widgets (
    id bigint PRIMARY KEY,
    name text NOT NULL
);

-- +goose Down
DROP TABLE widgets;
