-- ZS-SVC-Z-001 RB-05 / DQC-017 (docs/architecture/DQC_GAP_MATRIX.md, "blast radius of a revocation").
--
-- Revoking a certification withdraws reliance on it, but nothing recorded WHAT relied on it, so "what depended
-- on this?" had no data to answer from. A downstream service now records that it relied on a certification
-- (an append-only fact), and the blast radius of a revocation is that set.
--
-- The database holds the rules, not only the service: a reliance is immutable; it cannot be recorded against a
-- revoked certification, or one past its validity window; and one object relies on one certification once.

CREATE TABLE IF NOT EXISTS dq_certification_reliances (
    reliance_id         TEXT         PRIMARY KEY
        CHECK (reliance_id ~ '^dqr_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id           VARCHAR(64)  NOT NULL,
    certification_id    TEXT         NOT NULL REFERENCES dq_certifications(certification_id),
    relying_service     VARCHAR(128) NOT NULL CHECK (relying_service ~ '^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$'),
    relying_object_type VARCHAR(128) NOT NULL CHECK (relying_object_type ~ '^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$'),
    relying_object_id   VARCHAR(128) NOT NULL CHECK (relying_object_id ~ '^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$'),
    purpose             TEXT         NOT NULL CHECK (length(btrim(purpose)) > 0 AND length(purpose) <= 500),
    recorded_by         VARCHAR(128) NOT NULL CHECK (length(btrim(recorded_by)) > 0),
    recorded_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- Recording the same reliance twice is one fact, not two.
    UNIQUE (certification_id, relying_service, relying_object_type, relying_object_id)
);

CREATE INDEX IF NOT EXISTS idx_dq_cert_reliances_cert ON dq_certification_reliances (tenant_id, certification_id, recorded_at);

CREATE OR REPLACE FUNCTION enforce_dq_reliance_rules()
RETURNS trigger AS $$
DECLARE
    valid_to TIMESTAMPTZ;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'a certification reliance is immutable and cannot be changed or deleted';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM dq_certifications c WHERE c.certification_id = NEW.certification_id AND c.tenant_id = NEW.tenant_id) THEN
        RAISE EXCEPTION 'certification % does not belong to this tenant', NEW.certification_id;
    END IF;
    IF EXISTS (SELECT 1 FROM dq_certification_revocations v WHERE v.certification_id = NEW.certification_id) THEN
        RAISE EXCEPTION 'certification % is revoked and cannot be relied on', NEW.certification_id;
    END IF;
    SELECT c.valid_until INTO valid_to FROM dq_certifications c WHERE c.certification_id = NEW.certification_id;
    IF valid_to IS NOT NULL AND NEW.recorded_at > valid_to THEN
        RAISE EXCEPTION 'certification % is past its validity window and cannot be relied on', NEW.certification_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_dq_reliance_rules ON dq_certification_reliances;
CREATE TRIGGER trigger_dq_reliance_rules
    BEFORE INSERT OR UPDATE OR DELETE ON dq_certification_reliances
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_reliance_rules();

ALTER TABLE dq_certification_reliances ENABLE ROW LEVEL SECURITY;
ALTER TABLE dq_certification_reliances FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS dq_cert_reliances_tenant_isolation ON dq_certification_reliances;
CREATE POLICY dq_cert_reliances_tenant_isolation ON dq_certification_reliances
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- The reverse question ("what does this object rest on?") is a lookup by the relying object.
CREATE INDEX IF NOT EXISTS idx_dq_cert_reliances_subject
    ON dq_certification_reliances (tenant_id, relying_service, relying_object_type, relying_object_id);
