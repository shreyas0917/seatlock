-- Reverses 0004: back to JSONB (normalised storage).
ALTER TABLE idempotency_keys
    ALTER COLUMN response_body TYPE JSONB USING response_body::jsonb;
