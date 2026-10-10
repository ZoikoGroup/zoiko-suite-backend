-- ZS-SVC-D-001 AP-10 hardening: signer quorum, expiry, versioning, idempotency.

-- 1. Signer quorum. A high-value authorization needs several DISTINCT
--    signers (required_signatures); each signature is its own append-only
--    row, and the authorization becomes APPROVED only when the count is
--    reached. The unique index is what makes "the same person signs twice"
--    structurally impossible.
-- 2. Expiry. expires_at is set when the authorization is requested; a
--    background sweeper expires overdue PENDING/APPROVED rows. Rows created
--    before this migration keep expires_at NULL (never auto-expired) — the
--    sweeper ignores NULL, so an in-flight approval is not killed by a
--    deploy. Backfill them deliberately if that is wanted.
-- 3. version, bumped by a trigger on every update, so a caller can state
--    which version of the authorization it acted on (expected_version).
ALTER TABLE payment_authorizations
    ADD COLUMN version             INT NOT NULL DEFAULT 1,
    ADD COLUMN required_signatures INT NOT NULL DEFAULT 1 CHECK (required_signatures >= 1),
    ADD COLUMN signature_count     INT NOT NULL DEFAULT 0 CHECK (signature_count >= 0),
    ADD COLUMN expires_at          TIMESTAMPTZ NULL;

CREATE INDEX idx_payment_authorizations_expiry
    ON payment_authorizations (expires_at) WHERE status IN ('PENDING', 'APPROVED') AND expires_at IS NOT NULL;

CREATE OR REPLACE FUNCTION bump_authorization_version() RETURNS TRIGGER AS $$
BEGIN
    NEW.version := OLD.version + 1;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_bump_authorization_version
    BEFORE UPDATE ON payment_authorizations
    FOR EACH ROW EXECUTE FUNCTION bump_authorization_version();

CREATE TABLE authorization_signatures (
    signature_id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NULL,
    authorization_id      UUID NOT NULL REFERENCES payment_authorizations (authorization_id),
    signer_principal_id   TEXT NOT NULL,
    policy_result         TEXT NOT NULL DEFAULT '',
    policy_version_id     TEXT NOT NULL DEFAULT '',
    signed_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_authorization_signatures_signer
    ON authorization_signatures (authorization_id, signer_principal_id);

CREATE TRIGGER trg_reject_authorization_signatures_mutation
    BEFORE UPDATE OR DELETE ON authorization_signatures
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

ALTER TABLE authorization_signatures ENABLE ROW LEVEL SECURITY;
ALTER TABLE authorization_signatures FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON authorization_signatures
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

-- 4. Idempotency. One row per (tenant, command scope, Idempotency-Key). The
--    request hash binds the key to one exact request; status_code/response
--    are filled once the command finishes. tenant_key is '' for a NULL
--    tenant so the key can be part of the primary key.
CREATE TABLE idempotency_keys (
    tenant_key     TEXT NOT NULL,
    scope          TEXT NOT NULL,
    idem_key       TEXT NOT NULL,
    request_hash   TEXT NOT NULL,
    status_code    INT NULL,
    response_body  BYTEA NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at   TIMESTAMPTZ NULL,
    PRIMARY KEY (tenant_key, scope, idem_key)
);

CREATE INDEX idx_idempotency_keys_created ON idempotency_keys (created_at);
