-- 000018_idempotency_keys.up.sql
-- Idempotency-Key replay for admin writes (API standard; Doc 03 §3.7). The
-- envelope has always demanded the header on every write, and nothing read
-- it: two different keys with one body were one request, and one key reused
-- with a different body was processed as new. A key is scoped to the
-- principal and the exact method + path, kept for 24 hours, and holds the
-- first response so a retry gets it back byte for byte.

CREATE TABLE IF NOT EXISTS idempotency_keys (
    principal_id     VARCHAR(255) NOT NULL,
    idempotency_key  VARCHAR(255) NOT NULL,
    method           VARCHAR(8)   NOT NULL,
    path             TEXT         NOT NULL,
    -- sha256 of method, path, approval reference and body.
    request_hash     VARCHAR(71)  NOT NULL,
    -- NULL while the first request is still in flight.
    response_status  INTEGER,
    content_type     TEXT,
    response_body    BYTEA,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (principal_id, idempotency_key, method, path)
);

CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created ON idempotency_keys (created_at);

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO app_jurisdiction_rules;
    END IF;
END $$;
