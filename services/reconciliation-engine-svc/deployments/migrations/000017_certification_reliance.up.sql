-- ZS-SVC-Z-001 RB-05 / DQC-017 (docs/architecture/DQC_GAP_MATRIX.md, "blast radius of a revocation").
--
-- Revoking a certification withdraws reliance on it, but nothing recorded WHAT relied on it. A downstream service now
-- records that it relied on a certification (an append-only fact); the blast radius of a revocation is that set.
--
-- The database holds the rules: a reliance is immutable; it can only be recorded while the certification's run is
-- CERTIFIED (so never against a revoked or superseded certification); and one object relies on one certification once.

CREATE TABLE IF NOT EXISTS reconciliation_certification_reliances (
    reliance_id         TEXT         PRIMARY KEY
        CHECK (reliance_id ~ '^rrl_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id           VARCHAR(64)  NOT NULL,
    certification_id    TEXT         NOT NULL REFERENCES certifications(certification_id),
    relying_service     VARCHAR(128) NOT NULL CHECK (relying_service ~ '^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$'),
    relying_object_type VARCHAR(128) NOT NULL CHECK (relying_object_type ~ '^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$'),
    relying_object_id   VARCHAR(128) NOT NULL CHECK (relying_object_id ~ '^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$'),
    purpose             TEXT         NOT NULL CHECK (length(btrim(purpose)) > 0 AND length(purpose) <= 500),
    recorded_by         VARCHAR(128) NOT NULL CHECK (length(btrim(recorded_by)) > 0),
    recorded_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- Recording the same reliance twice is one fact, not two.
    UNIQUE (certification_id, relying_service, relying_object_type, relying_object_id)
);

CREATE INDEX IF NOT EXISTS idx_recon_cert_reliances_cert ON reconciliation_certification_reliances (tenant_id, certification_id, recorded_at);

CREATE OR REPLACE FUNCTION enforce_recon_reliance_rules()
RETURNS trigger AS $$
DECLARE
    cert_state TEXT;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'a certification reliance is immutable and cannot be changed or deleted';
    END IF;
    SELECT r.certification_state INTO cert_state
      FROM certifications c JOIN reconciliation_runs r ON r.run_id = c.run_id
     WHERE c.certification_id = NEW.certification_id AND c.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'certification % does not belong to this tenant', NEW.certification_id;
    END IF;
    IF cert_state <> 'CERTIFIED' THEN
        RAISE EXCEPTION 'certification % is % and cannot be relied on', NEW.certification_id, cert_state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_recon_reliance_rules ON reconciliation_certification_reliances;
CREATE TRIGGER trigger_recon_reliance_rules
    BEFORE INSERT OR UPDATE OR DELETE ON reconciliation_certification_reliances
    FOR EACH ROW EXECUTE FUNCTION enforce_recon_reliance_rules();

ALTER TABLE reconciliation_certification_reliances ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_certification_reliances FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS recon_cert_reliances_tenant_isolation ON reconciliation_certification_reliances;
CREATE POLICY recon_cert_reliances_tenant_isolation ON reconciliation_certification_reliances
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- The reverse question ("what does this object rest on?") is a lookup by the relying object.
CREATE INDEX IF NOT EXISTS idx_recon_cert_reliances_subject
    ON reconciliation_certification_reliances (tenant_id, relying_service, relying_object_type, relying_object_id);
