-- ZS-SVC-L-001 WFP-01 — Worker Financial Profile Interface.
--
-- A versioned, effective-dated MIRROR of the minimum financial attributes the
-- payroll interfaces need. It is not the HR master: nothing here is added to
-- the employees table, and no value is defaulted from the employee or the
-- legal entity — a missing jurisdiction, currency or entity mapping blocks the
-- profile (it is quarantined, never filled in).

CREATE TABLE IF NOT EXISTS wfp_worker_financial_profiles (
    profile_id                UUID PRIMARY KEY,
    tenant_id                 TEXT NOT NULL,
    worker_id                 UUID NOT NULL,
    assignment_id             TEXT NOT NULL,
    legal_entity_id           TEXT NOT NULL,
    payroll_jurisdiction_id   TEXT NOT NULL,
    pay_currency              CHAR(3) NOT NULL,
    employment_classification TEXT NOT NULL,
    cost_center               TEXT,
    dimensions                JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Opaque reference to the approved payee master identity. Not verified
    -- against payee-banking-identity-svc until WFP-07 (Wave 7).
    payee_ref                 TEXT,
    payee_ref_status          TEXT NOT NULL DEFAULT 'NONE',
    payee_ref_changed         BOOLEAN NOT NULL DEFAULT false,
    effective_from            DATE NOT NULL,
    effective_to              DATE,
    source_system_id          TEXT NOT NULL,
    source_version            INTEGER NOT NULL,
    source_fingerprint        TEXT NOT NULL,
    state                     TEXT NOT NULL,
    quarantine_reason         TEXT,
    supersedes_profile_id     UUID REFERENCES wfp_worker_financial_profiles (profile_id),
    version                   INTEGER NOT NULL DEFAULT 1,
    correlation_id            TEXT NOT NULL,
    created_by                TEXT NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_wfp_state_check CHECK (state IN ('ACTIVE', 'SUPERSEDED', 'DEACTIVATED', 'QUARANTINED')),
    CONSTRAINT wfp_wfp_payee_status_check CHECK (payee_ref_status IN ('NONE', 'UNVERIFIED', 'VERIFIED')),
    CONSTRAINT wfp_wfp_payee_consistency CHECK ((payee_ref IS NULL) = (payee_ref_status = 'NONE')),
    CONSTRAINT wfp_wfp_currency_check CHECK (pay_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wfp_wfp_window_check CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT wfp_wfp_source_version_check CHECK (source_version >= 1),
    CONSTRAINT wfp_wfp_quarantine_reason CHECK ((state = 'QUARANTINED') = (quarantine_reason IS NOT NULL)),
    -- A source version is one identity, however many times it is delivered.
    CONSTRAINT wfp_wfp_unique_source_version UNIQUE (tenant_id, source_system_id, worker_id, assignment_id, source_version)
);
CREATE INDEX IF NOT EXISTS idx_wfp_wfp_worker
    ON wfp_worker_financial_profiles (tenant_id, worker_id, assignment_id, effective_from DESC);

-- WFP-CTRL-002: no two ACTIVE profiles for one worker assignment may cover the
-- same date. Enforced here rather than only in Go, so a racing writer cannot
-- produce contradictory financial context.
CREATE OR REPLACE FUNCTION wfp_wfp_no_overlap() RETURNS trigger AS $$
BEGIN
    IF NEW.state = 'ACTIVE' AND EXISTS (
        SELECT 1 FROM wfp_worker_financial_profiles p
        WHERE p.tenant_id = NEW.tenant_id AND p.worker_id = NEW.worker_id AND p.assignment_id = NEW.assignment_id
          AND p.state = 'ACTIVE' AND p.profile_id <> NEW.profile_id
          AND daterange(p.effective_from, p.effective_to, '[)') && daterange(NEW.effective_from, NEW.effective_to, '[)')
    ) THEN
        RAISE EXCEPTION 'overlapping ACTIVE worker financial profile for this assignment'
            USING ERRCODE = 'exclusion_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS wfp_wfp_no_overlap_trg ON wfp_worker_financial_profiles;
CREATE TRIGGER wfp_wfp_no_overlap_trg
    BEFORE INSERT OR UPDATE ON wfp_worker_financial_profiles
    FOR EACH ROW EXECUTE FUNCTION wfp_wfp_no_overlap();

-- A profile's content is evidence. Only its lifecycle (state, end date,
-- version) may change; a changed fact is a new source version.
CREATE OR REPLACE FUNCTION wfp_wfp_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'worker financial profiles are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.worker_id IS DISTINCT FROM OLD.worker_id
       OR NEW.assignment_id IS DISTINCT FROM OLD.assignment_id
       OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
       OR NEW.payroll_jurisdiction_id IS DISTINCT FROM OLD.payroll_jurisdiction_id
       OR NEW.pay_currency IS DISTINCT FROM OLD.pay_currency
       OR NEW.employment_classification IS DISTINCT FROM OLD.employment_classification
       OR NEW.cost_center IS DISTINCT FROM OLD.cost_center
       OR NEW.dimensions IS DISTINCT FROM OLD.dimensions
       OR NEW.payee_ref IS DISTINCT FROM OLD.payee_ref
       OR NEW.effective_from IS DISTINCT FROM OLD.effective_from
       OR NEW.source_system_id IS DISTINCT FROM OLD.source_system_id
       OR NEW.source_version IS DISTINCT FROM OLD.source_version
       OR NEW.source_fingerprint IS DISTINCT FROM OLD.source_fingerprint THEN
        RAISE EXCEPTION 'worker financial profile content is immutable; supply a new source_version'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state IN ('SUPERSEDED', 'DEACTIVATED') AND NEW.state IS DISTINCT FROM OLD.state THEN
        RAISE EXCEPTION '% is terminal', OLD.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS wfp_wfp_guard_trg ON wfp_worker_financial_profiles;
CREATE TRIGGER wfp_wfp_guard_trg
    BEFORE UPDATE OR DELETE ON wfp_worker_financial_profiles
    FOR EACH ROW EXECUTE FUNCTION wfp_wfp_guard();

-- Transactional outbox (same shape as payroll-run-svc / accounting-period-svc).
CREATE TABLE IF NOT EXISTS wfp_outbox (
    outbox_id     BIGSERIAL PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    object_id     TEXT NOT NULL,
    event_type    TEXT NOT NULL,
    payload       JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at  TIMESTAMPTZ,
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT
);
CREATE INDEX IF NOT EXISTS idx_wfp_outbox_unpublished
    ON wfp_outbox (created_at, outbox_id) WHERE published_at IS NULL;

ALTER TABLE wfp_worker_financial_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_worker_financial_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_worker_financial_profiles FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_tenant_isolation ON wfp_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
