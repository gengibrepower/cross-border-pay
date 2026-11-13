-- +goose Up
-- A card attempt keeps the payer's card token, a fictitious one (section 7: no card
-- number is ever stored), so a request that died before calling the acquirer can be
-- completed by its retry. The CHECK refuses anything that is not a token.
ALTER TABLE payment_attempts
    ADD COLUMN card_token text CHECK (card_token ~ '^tok_[A-Za-z0-9_]{1,60}$');

-- +goose Down
ALTER TABLE payment_attempts DROP COLUMN card_token;
