-- +goose Up
-- A card attempt either captures as it authorizes or waits for the merchant's capture
-- (decision 18); only a card attempt with a token can wait.
ALTER TABLE payment_attempts
    ADD COLUMN manual_capture boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT payment_attempts_manual_capture_card CHECK (NOT manual_capture OR card_token IS NOT NULL);

-- The quote of what a capture took: the payment's own quote for the whole amount, or
-- one priced at the same base rate for a part. Written before the acquirer is called,
-- so a capture whose answer is lost is confirmed against it. Everything that posts the
-- payment's money reads coalesce(captured_quote_id, quote_id).
ALTER TABLE payments
    ADD COLUMN captured_quote_id uuid REFERENCES quotes (id),
    ADD CONSTRAINT payments_captured_card CHECK (captured_quote_id IS NULL OR method = 'card');

-- +goose Down
ALTER TABLE payments DROP COLUMN captured_quote_id;
ALTER TABLE payment_attempts DROP COLUMN manual_capture;
