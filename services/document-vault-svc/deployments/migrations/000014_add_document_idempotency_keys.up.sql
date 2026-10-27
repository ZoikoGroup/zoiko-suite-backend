-- 000014_add_document_idempotency_keys.up.sql
--
-- Replay protection for the two commands that accept document bytes
-- (POST /v1/documents and POST /v1/documents/{id}/versions). ZS-SVC-S-001 §9.1:
-- "Every mutating command accepts/derives operation_id and idempotency key.
-- Retried commands return the prior authoritative result where safe", and
-- DRC-I30. The Idempotency-Key header was already REQUIRED by the canonical
-- service input contract, but nothing used it: a client retrying after a
-- timeout created a second document or version.
--
-- The claim row is written in the SAME transaction as the document/version, so
-- it exists if and only if the operation committed; a retry either finds it
-- (and gets the original response) or the operation never happened.
--
-- Scope is (tenant, principal, key): a key never replays another principal's
-- response. request_hash binds the key to the request (method + path + body):
-- the same key with a different request is refused, not replayed. The
-- specification does not state the scope or the binding; this is the platform's
-- usual reading of INV-08 and is recorded as such in the audit file.
CREATE TABLE IF NOT EXISTS document_idempotency_keys (
    tenant_id        UUID         NOT NULL,
    principal_id     VARCHAR(255) NOT NULL CHECK (principal_id <> ''),
    idempotency_key  VARCHAR(255) NOT NULL CHECK (idempotency_key <> ''),
    operation        VARCHAR(64)  NOT NULL,
    request_hash     CHAR(64)     NOT NULL,
    response_status  INT          NOT NULL,
    response_body    JSONB        NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, principal_id, idempotency_key)
);

ALTER TABLE document_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON document_idempotency_keys
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

-- A stored outcome is permanent: it is what every later retry is answered with.
CREATE OR REPLACE FUNCTION reject_idempotency_key_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'document_idempotency_keys rows are immutable: % is not permitted', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_reject_idempotency_key_mutation ON document_idempotency_keys;
CREATE TRIGGER trg_reject_idempotency_key_mutation
    BEFORE UPDATE OR DELETE ON document_idempotency_keys
    FOR EACH ROW EXECUTE FUNCTION reject_idempotency_key_mutation();
