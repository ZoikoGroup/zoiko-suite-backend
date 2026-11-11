-- 000020_registrations.up.sql
--
-- ORG-09 Registration & License (ZS-SVC-I-001 s4.9, ZS-SVC-U-001 s13, s14.1).
--
-- A registration is generic evidence that a holder (a legal entity or a party) is registered or licensed by an
-- authority: company, payroll, permit, licence, regulated and tax-reference records. It records what an authority
-- said and the lifecycle of that evidence. It decides no tax semantics (TAX-01 owns those), no legal sufficiency and
-- no nexus. An unverified registration is DRAFT and cannot be ACTIVE: the table refuses it. Every change is an
-- append-only history row, and nothing is deleted.

CREATE TABLE registrations (
    registration_id        UUID PRIMARY KEY,
    tenant_id              UUID NOT NULL REFERENCES tenants(tenant_id),
    registration_type      VARCHAR(24) NOT NULL,
    issuing_authority      TEXT NOT NULL,
    authority_normalized   TEXT NOT NULL,
    jurisdiction           VARCHAR(16) NOT NULL,
    number                 TEXT NOT NULL,
    number_normalized      TEXT NOT NULL,
    holder_legal_entity_id UUID REFERENCES legal_entities(legal_entity_id),
    holder_party_id        UUID,
    status                 VARCHAR(10) NOT NULL DEFAULT 'DRAFT',
    -- LICENSE, PERMIT and REGULATED can carry legal consequences: independent verification and renewal.
    material               BOOLEAN NOT NULL,
    issue_date             DATE,
    effective_from         DATE NOT NULL,
    expiry_date            DATE,
    source_class           VARCHAR(24) NOT NULL,
    evidence_ref           TEXT NOT NULL,
    evidence_hash          VARCHAR(64),
    supersedes_id          UUID,
    superseded_by_id       UUID,
    record_version         BIGINT NOT NULL DEFAULT 1,
    created_by             VARCHAR(255) NOT NULL,
    created_at             TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    verified_by            VARCHAR(255),
    verified_at            TIMESTAMP WITH TIME ZONE,
    -- Who last made the registration valid (verified or renewed it): a material renewal is by someone else.
    last_grant_by          VARCHAR(255),
    -- The expiry date a warning has been sent for, so a window warns once and a renewal warns again.
    expiring_notified_for  DATE,
    create_request_id      VARCHAR(128),
    fingerprint            TEXT,
    correlation_id         VARCHAR(255),
    CONSTRAINT reg_tenant_unique UNIQUE (tenant_id, registration_id),
    CONSTRAINT reg_party_in_tenant FOREIGN KEY (tenant_id, holder_party_id) REFERENCES parties (tenant_id, party_id),
    CONSTRAINT reg_one_holder CHECK ((holder_legal_entity_id IS NULL) <> (holder_party_id IS NULL)),
    CONSTRAINT reg_type_known CHECK (registration_type IN
        ('COMPANY_REGISTRATION', 'TAX_REFERENCE', 'EMPLOYER_PAYROLL', 'PERMIT', 'LICENSE', 'REGULATED', 'OTHER_GOVERNED')),
    CONSTRAINT reg_status_known CHECK (status IN ('DRAFT', 'ACTIVE', 'SUSPENDED', 'EXPIRED', 'REVOKED', 'SUPERSEDED')),
    CONSTRAINT reg_source_known CHECK (source_class IN
        ('AUTHORITY_REGISTRY', 'AUTHORITY_DOCUMENT', 'TENANT_ASSERTED', 'SYSTEM_IMPORT', 'EXTERNAL_PROVIDER')),
    CONSTRAINT reg_present CHECK (btrim(issuing_authority) <> '' AND btrim(jurisdiction) <> '' AND btrim(number) <> ''
        AND number_normalized <> '' AND btrim(evidence_ref) <> ''),
    CONSTRAINT reg_dates CHECK ((expiry_date IS NULL OR expiry_date >= effective_from)
        AND (issue_date IS NULL OR issue_date <= effective_from)),
    -- s4.9: an unverifiable registration remains unverified and cannot be treated as active proof.
    CONSTRAINT reg_verified_to_be_valid CHECK (status IN ('DRAFT', 'REVOKED') OR verified_at IS NOT NULL),
    -- s4.9: no single operator creates and verifies a material regulated registration.
    CONSTRAINT reg_independent_verification CHECK (verified_by IS NULL OR NOT material OR verified_by <> created_by),
    CONSTRAINT reg_version_positive CHECK (record_version > 0),
    CONSTRAINT reg_hash_shape CHECK (evidence_hash IS NULL OR evidence_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT reg_request_has_fingerprint CHECK (create_request_id IS NULL OR fingerprint IS NOT NULL)
);
-- Authority + type + number + jurisdiction names one live registration (s4.9 candidate uniqueness).
CREATE UNIQUE INDEX reg_one_live_number ON registrations (tenant_id, registration_type, authority_normalized, jurisdiction, number_normalized)
    WHERE status IN ('DRAFT', 'ACTIVE', 'SUSPENDED');
CREATE UNIQUE INDEX reg_one_per_request ON registrations (tenant_id, create_request_id) WHERE create_request_id IS NOT NULL;
CREATE INDEX idx_reg_holder_entity ON registrations (tenant_id, holder_legal_entity_id) WHERE holder_legal_entity_id IS NOT NULL;
CREATE INDEX idx_reg_holder_party ON registrations (tenant_id, holder_party_id) WHERE holder_party_id IS NOT NULL;
CREATE INDEX idx_reg_expiry ON registrations (expiry_date) WHERE status IN ('ACTIVE', 'SUSPENDED') AND expiry_date IS NOT NULL;
CREATE INDEX idx_reg_number ON registrations (tenant_id, number_normalized);

-- A registration is held only by a legal entity of its own tenant (FK checks bypass row-level security).
CREATE OR REPLACE FUNCTION registration_holder_tenant_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.holder_legal_entity_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM legal_entities WHERE legal_entity_id = NEW.holder_legal_entity_id AND tenant_id = NEW.tenant_id) THEN
        RAISE EXCEPTION 'a registration is held only by a legal entity of its own tenant' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER registration_holder_tenant_guard BEFORE INSERT OR UPDATE OF holder_legal_entity_id, tenant_id ON registrations
    FOR EACH ROW EXECUTE FUNCTION registration_holder_tenant_guard();

-- What happened to a registration, in order: the lifecycle, the dates after each step and who did it.
CREATE TABLE registration_history (
    history_id      UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL,
    registration_id UUID NOT NULL,
    seq             INTEGER NOT NULL,
    command         VARCHAR(24) NOT NULL,
    from_status     VARCHAR(10),
    to_status       VARCHAR(10) NOT NULL,
    effective_date  DATE NOT NULL,
    expiry_after    DATE,
    reason          TEXT,
    evidence_ref    TEXT NOT NULL,
    evidence_hash   VARCHAR(64),
    actor           VARCHAR(255) NOT NULL,
    version_before  BIGINT,
    version_after   BIGINT NOT NULL,
    occurred_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id  VARCHAR(255),
    CONSTRAINT rh_reg_in_tenant FOREIGN KEY (tenant_id, registration_id) REFERENCES registrations (tenant_id, registration_id),
    CONSTRAINT rh_seq_unique UNIQUE (registration_id, seq),
    CONSTRAINT rh_evidence CHECK (btrim(evidence_ref) <> '')
);
CREATE INDEX idx_rh_reg ON registration_history (tenant_id, registration_id, seq);

CREATE OR REPLACE FUNCTION registration_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'registrations are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.registration_type <> OLD.registration_type OR NEW.issuing_authority <> OLD.issuing_authority
       OR NEW.jurisdiction <> OLD.jurisdiction OR NEW.number <> OLD.number OR NEW.number_normalized <> OLD.number_normalized
       OR NEW.holder_legal_entity_id IS DISTINCT FROM OLD.holder_legal_entity_id OR NEW.holder_party_id IS DISTINCT FROM OLD.holder_party_id
       OR NEW.material <> OLD.material OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a registration''s identity is immutable; supersede it with a new one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status IN ('REVOKED', 'SUPERSEDED') THEN
        RAISE EXCEPTION 'a % registration cannot change', OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER registration_guard BEFORE UPDATE OR DELETE ON registrations
    FOR EACH ROW EXECUTE FUNCTION registration_guard();

CREATE OR REPLACE FUNCTION registration_history_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'registration history is append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER registration_history_guard BEFORE UPDATE OR DELETE ON registration_history
    FOR EACH ROW EXECUTE FUNCTION registration_history_guard();

ALTER TABLE registrations ENABLE ROW LEVEL SECURITY;
ALTER TABLE registrations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON registrations FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
-- The expiry sweep finds which tenants have registrations due, across tenants, by a named capability: read only,
-- and only the rows that are due. Everything it then does runs inside one tenant's own policy.
CREATE POLICY expiry_sweep_read_policy ON registrations FOR SELECT
    USING (current_setting('app.registration_sweep', true) = 'true' AND status IN ('ACTIVE', 'SUSPENDED') AND expiry_date IS NOT NULL);

ALTER TABLE registration_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE registration_history FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON registration_history FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
