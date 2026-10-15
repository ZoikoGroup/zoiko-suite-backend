-- ZS-SVC-L-001 Wave 1 — shared worker/run identity, source trust profiles,
-- payroll rule-manifest references, and the transactional outbox.
--
-- Nothing here changes an existing table. payroll_runs/pay_slips keep their
-- shape; the WFP objects reference a payroll_runs row through run_ref.payroll_run_id
-- once WFP-04 links them (Wave 4).

-- ── Source trust profiles (WFP-D-01) ─────────────────────────────────────────
-- Which source systems may submit which classes of payroll fact for a tenant.
-- A source with no ACTIVE profile covering a capability cannot submit it.
CREATE TABLE IF NOT EXISTS wfp_source_trust_profiles (
    profile_id            UUID PRIMARY KEY,
    tenant_id             TEXT NOT NULL,
    source_system_id      TEXT NOT NULL,
    source_kind           TEXT NOT NULL,
    status                TEXT NOT NULL,
    allowed_capabilities  TEXT[] NOT NULL,
    certification_ref     TEXT,
    certified_at          TIMESTAMPTZ,
    version               INTEGER NOT NULL DEFAULT 1,
    created_by            TEXT NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_trust_kind_check CHECK (source_kind IN
        ('ZOIKO_PAYROLL', 'EXTERNAL_PAYROLL_PROVIDER', 'ZOIKO_HR', 'ZOIKO_TIME', 'APPROVED_EXTERNAL')),
    CONSTRAINT wfp_trust_status_check CHECK (status IN ('PENDING_CERTIFICATION', 'ACTIVE', 'SUSPENDED', 'RETIRED')),
    CONSTRAINT wfp_trust_capabilities_check CHECK (
        cardinality(allowed_capabilities) > 0
        AND allowed_capabilities <@ ARRAY['WORKER_PROFILE', 'TIME_LEAVE', 'PAYROLL_INPUT', 'PAYROLL_RESULT']::text[]),
    -- A source is trusted only on certification evidence; ACTIVE without it is unrepresentable.
    CONSTRAINT wfp_trust_active_needs_certification CHECK (
        status <> 'ACTIVE' OR (certification_ref IS NOT NULL AND certified_at IS NOT NULL)),
    CONSTRAINT wfp_trust_unique_source UNIQUE (tenant_id, source_system_id)
);

-- ── Payroll rule-manifest references ─────────────────────────────────────────
-- The exact, effective-dated calculation/rule configuration a source engine
-- used. Append-only: a changed manifest is a NEW row (new version/digest),
-- never an edit, so a historical run can always be replayed against what it used.
CREATE TABLE IF NOT EXISTS wfp_payroll_rule_manifests (
    manifest_id        UUID PRIMARY KEY,
    tenant_id          TEXT NOT NULL,
    source_system_id   TEXT NOT NULL,
    jurisdiction_id    TEXT NOT NULL,
    manifest_version   TEXT NOT NULL,
    manifest_digest    TEXT NOT NULL,
    effective_from     DATE NOT NULL,
    effective_to       DATE,
    evidence_ref       TEXT NOT NULL,
    registered_by      TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_manifest_digest_check CHECK (manifest_digest ~ '^[0-9a-f]{64}$'),
    CONSTRAINT wfp_manifest_window_check CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT wfp_manifest_unique UNIQUE (tenant_id, source_system_id, jurisdiction_id, manifest_version, manifest_digest)
);
CREATE INDEX IF NOT EXISTS idx_wfp_manifest_resolve
    ON wfp_payroll_rule_manifests (tenant_id, source_system_id, jurisdiction_id, effective_from DESC);

-- ── Payroll run references (canonical run identity, spec §2.2) ───────────────
-- Identity is tenant + entity + source system + source run + source version +
-- period + pay date + rule manifest (+ result fingerprint once WFP-04 accepts).
-- "payroll period + employee" is deliberately NOT a key: retro, off-cycle and
-- parallel payroll groups make it unsafe.
CREATE TABLE IF NOT EXISTS wfp_payroll_run_refs (
    run_ref_id             UUID PRIMARY KEY,
    tenant_id              TEXT NOT NULL,
    legal_entity_id        TEXT NOT NULL,
    source_system_id       TEXT NOT NULL,
    source_run_id          TEXT NOT NULL,
    source_run_version     INTEGER NOT NULL,
    run_kind               TEXT NOT NULL,
    payroll_period_id      TEXT NOT NULL,
    pay_date               DATE NOT NULL,
    rule_manifest_id       UUID NOT NULL REFERENCES wfp_payroll_rule_manifests (manifest_id),
    payroll_run_id         UUID,
    acceptance_state       TEXT NOT NULL DEFAULT 'RECEIVED',
    supersedes_run_ref_id  UUID REFERENCES wfp_payroll_run_refs (run_ref_id),
    identity_fingerprint   TEXT NOT NULL,
    result_fingerprint     TEXT,
    correlation_id         TEXT NOT NULL,
    causation_id           TEXT,
    version                INTEGER NOT NULL DEFAULT 1,
    created_by             TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_run_ref_kind_check CHECK (run_kind IN ('REGULAR', 'OFF_CYCLE', 'RETRO', 'CORRECTION')),
    CONSTRAINT wfp_run_ref_state_check CHECK (acceptance_state IN
        ('RECEIVED', 'VALIDATING', 'ACCEPTED', 'REJECTED', 'QUARANTINED', 'SUPERSEDED')),
    CONSTRAINT wfp_run_ref_version_check CHECK (source_run_version >= 1),
    -- The same source run version is one identity, however many times it is delivered.
    CONSTRAINT wfp_run_ref_unique_version UNIQUE (tenant_id, legal_entity_id, source_system_id, source_run_id, source_run_version),
    CONSTRAINT wfp_run_ref_unique_fingerprint UNIQUE (tenant_id, identity_fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_wfp_run_ref_source
    ON wfp_payroll_run_refs (tenant_id, source_system_id, source_run_id, source_run_version DESC);

-- ── Guards ───────────────────────────────────────────────────────────────────
-- Rule manifests are evidence: no UPDATE, no DELETE.
CREATE OR REPLACE FUNCTION wfp_manifest_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'wfp_payroll_rule_manifests is append-only (% blocked)', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS wfp_manifest_no_change ON wfp_payroll_rule_manifests;
CREATE TRIGGER wfp_manifest_no_change
    BEFORE UPDATE OR DELETE ON wfp_payroll_rule_manifests
    FOR EACH ROW EXECUTE FUNCTION wfp_manifest_immutable();

-- A run ref's identity never changes, an ACCEPTED result is immutable except
-- for being superseded, and the result fingerprint is set once.
CREATE OR REPLACE FUNCTION wfp_run_ref_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'wfp_payroll_run_refs rows are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
       OR NEW.source_system_id IS DISTINCT FROM OLD.source_system_id
       OR NEW.source_run_id IS DISTINCT FROM OLD.source_run_id
       OR NEW.source_run_version IS DISTINCT FROM OLD.source_run_version
       OR NEW.run_kind IS DISTINCT FROM OLD.run_kind
       OR NEW.payroll_period_id IS DISTINCT FROM OLD.payroll_period_id
       OR NEW.pay_date IS DISTINCT FROM OLD.pay_date
       OR NEW.rule_manifest_id IS DISTINCT FROM OLD.rule_manifest_id
       OR NEW.identity_fingerprint IS DISTINCT FROM OLD.identity_fingerprint
       OR NEW.supersedes_run_ref_id IS DISTINCT FROM OLD.supersedes_run_ref_id THEN
        RAISE EXCEPTION 'payroll run identity is immutable; a changed run is a new source_run_version'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.result_fingerprint IS NOT NULL AND NEW.result_fingerprint IS DISTINCT FROM OLD.result_fingerprint THEN
        RAISE EXCEPTION 'result_fingerprint is set once'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.acceptance_state = 'ACCEPTED' AND NEW.acceptance_state NOT IN ('ACCEPTED', 'SUPERSEDED') THEN
        RAISE EXCEPTION 'an ACCEPTED payroll result is immutable; it can only be superseded'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.acceptance_state IN ('REJECTED', 'SUPERSEDED') AND NEW.acceptance_state IS DISTINCT FROM OLD.acceptance_state THEN
        RAISE EXCEPTION '% is terminal', OLD.acceptance_state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS wfp_run_ref_guard_trg ON wfp_payroll_run_refs;
CREATE TRIGGER wfp_run_ref_guard_trg
    BEFORE UPDATE OR DELETE ON wfp_payroll_run_refs
    FOR EACH ROW EXECUTE FUNCTION wfp_run_ref_guard();

-- ── Transactional outbox ─────────────────────────────────────────────────────
-- WFP events are inserted in the same transaction as the state change and
-- drained to Kafka by internal/outbox. The payload is the fully built envelope.
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

-- ── Row-level security ───────────────────────────────────────────────────────
-- NULLIF because a pooled connection keeps a custom GUC as '' after a
-- transaction-local SET resets.
ALTER TABLE wfp_source_trust_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_source_trust_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_source_trust_profiles FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_payroll_rule_manifests ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payroll_rule_manifests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_rule_manifests FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_payroll_run_refs ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payroll_run_refs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_run_refs FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Request-path writes are confined to the actor's tenant; the relay is not a
-- request, installs app.outbox_relay, and is admitted by the second disjunct.
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
