-- mtls-management-svc: authoritative certificate records and communication
-- policies (tracker rows 8m / 84d / 88 — this service previously kept both in
-- an in-memory map, so every restart forgot which certificates existed, which
-- had been rotated and which were revoked).
--
-- What is authoritative here and what is not:
--   * mtls_certificates  — the record of every leaf certificate the internal CA
--     issued under a tenant: identity (service/common name, legal entity), the
--     current serial + every superseded serial (prior_serial_numbers), the
--     public PEM, validity window and lifecycle status. The leaf PRIVATE KEY is
--     never stored (it is returned once, at issuance/rotation).
--   * mtls_communication_policies — tenant-wide service-to-service policies.
--   * NOT here: the CA key/cert and the revocation ledger behind GET /v1/mtls/crl.
--     Both stay on the CA data volume (CA_DATA_DIR) — the CRL is a platform-wide,
--     tenant-less artefact signed by that key, and the ledger is written before
--     the record transition so a crash can only over-revoke, never forget one.

CREATE TABLE IF NOT EXISTS mtls_certificates (
    id                   UUID         PRIMARY KEY,
    tenant_id            VARCHAR(255) NOT NULL,
    legal_entity_id      VARCHAR(255) NOT NULL,
    service_name         VARCHAR(255) NOT NULL,
    common_name          VARCHAR(255) NOT NULL,
    issuer               VARCHAR(255) NOT NULL,
    serial_number        VARCHAR(64)  NOT NULL,
    fingerprint          VARCHAR(128) NOT NULL,
    certificate_pem      TEXT         NOT NULL,
    prior_serial_numbers TEXT[]       NOT NULL DEFAULT '{}',
    valid_from           TIMESTAMPTZ  NOT NULL,
    valid_to             TIMESTAMPTZ  NOT NULL,
    rotation_days        INT          NOT NULL CHECK (rotation_days > 0),
    auto_rotate          BOOLEAN      NOT NULL DEFAULT false,
    status               VARCHAR(16)  NOT NULL CHECK (status IN ('ACTIVE', 'EXPIRED', 'REVOKED', 'PENDING')),
    revoked_at           TIMESTAMPTZ,
    -- Incremented on every lifecycle transition (rotate, revoke).
    version              BIGINT       NOT NULL DEFAULT 1,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CHECK ((status = 'REVOKED') = (revoked_at IS NOT NULL))
);

-- A serial is issued once by the CA (128-bit random); a second record carrying
-- the same current serial would make revocation ambiguous.
CREATE UNIQUE INDEX IF NOT EXISTS uq_mtls_certificates_serial ON mtls_certificates (serial_number);
CREATE INDEX IF NOT EXISTS idx_mtls_certificates_scope ON mtls_certificates (tenant_id, legal_entity_id, status);

-- A revoked record is terminal: it may not return to another status and may
-- not take on new key material. The service refuses both (rotate answers 409
-- certificate_revoked); this trigger makes the database refuse them too.
CREATE OR REPLACE FUNCTION mtls_certificates_revoked_is_terminal() RETURNS trigger AS $$
BEGIN
    IF OLD.status = 'REVOKED' AND (
           NEW.status <> 'REVOKED'
        OR NEW.serial_number <> OLD.serial_number
        OR NEW.prior_serial_numbers <> OLD.prior_serial_numbers
        OR NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
        RAISE EXCEPTION 'certificate % is revoked and cannot be changed', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_mtls_certificates_revoked_is_terminal ON mtls_certificates;
CREATE TRIGGER trg_mtls_certificates_revoked_is_terminal
    BEFORE UPDATE ON mtls_certificates
    FOR EACH ROW EXECUTE FUNCTION mtls_certificates_revoked_is_terminal();

CREATE TABLE IF NOT EXISTS mtls_communication_policies (
    id             UUID         PRIMARY KEY,
    tenant_id      VARCHAR(255) NOT NULL,
    policy_name    VARCHAR(255) NOT NULL,
    source_service VARCHAR(255) NOT NULL DEFAULT '',
    target_service VARCHAR(255) NOT NULL DEFAULT '',
    -- '' is accepted because the published contract makes action optional.
    action         VARCHAR(16)  NOT NULL DEFAULT '' CHECK (action IN ('', 'ALLOW', 'DENY')),
    requires_mtls  BOOLEAN      NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_mtls_communication_policies_tenant ON mtls_communication_policies (tenant_id, created_at);

-- Tenant isolation: every statement the service issues runs in a transaction
-- that first sets app.tenant_id. FORCE binds the table owner too.
ALTER TABLE mtls_certificates ENABLE ROW LEVEL SECURITY;
ALTER TABLE mtls_certificates FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS mtls_certificates_tenant_isolation ON mtls_certificates;
CREATE POLICY mtls_certificates_tenant_isolation ON mtls_certificates FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE mtls_communication_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE mtls_communication_policies FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS mtls_communication_policies_tenant_isolation ON mtls_communication_policies;
CREATE POLICY mtls_communication_policies_tenant_isolation ON mtls_communication_policies FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
