-- +goose Up
-- A boleto attempt keeps what it asked the bank to register, the payer (decision 54)
-- and the due date, so a request that died before calling the bank is completed by
-- its retry with the same boleto; and the barcode the bank returned, so GET shows the
-- boleto the payer may have printed. The linha digitável is derived from the barcode.
-- The CPF is stored bare, as the bank needs it, and is masked everywhere it is shown.
ALTER TABLE payment_attempts
    ADD COLUMN payer_name     text CHECK (length(payer_name) BETWEEN 1 AND 400),
    ADD COLUMN payer_document text CHECK (payer_document ~ '^[0-9]{11}$'),
    ADD COLUMN payer_email    text CHECK (length(payer_email) BETWEEN 3 AND 254),
    ADD COLUMN due_date       date,
    ADD COLUMN barcode        text CHECK (barcode ~ '^[0-9]{44}$'),
    ADD CONSTRAINT payment_attempts_boleto_complete CHECK (
        (payer_name IS NULL) = (payer_document IS NULL) AND (payer_name IS NULL) = (due_date IS NULL)),
    ADD CONSTRAINT payment_attempts_boleto_email CHECK (payer_email IS NULL OR payer_name IS NOT NULL),
    ADD CONSTRAINT payment_attempts_boleto_barcode CHECK (barcode IS NULL OR due_date IS NOT NULL),
    ADD CONSTRAINT payment_attempts_boleto_not_card CHECK (due_date IS NULL OR card_token IS NULL);

-- +goose Down
ALTER TABLE payment_attempts
    DROP COLUMN barcode,
    DROP COLUMN due_date,
    DROP COLUMN payer_email,
    DROP COLUMN payer_document,
    DROP COLUMN payer_name;
