-- 0004: store idempotent responses VERBATIM.
--
-- 0003 used JSONB for idempotency_keys.response_body. JSONB stores a parsed,
-- normalised form: it reorders object keys and changes whitespace. A replayed
-- response was therefore equal in meaning but NOT byte-for-byte identical to
-- the original, and an idempotent replay must be exactly the same response.
-- The plain JSON type stores the input text as-is (it is still validated as
-- JSON), so the replay is identical.
ALTER TABLE idempotency_keys
    ALTER COLUMN response_body TYPE JSON USING response_body::json;
