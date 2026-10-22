-- 004 ZS-SVC-T-001 XIC-01: Integration Contract & Connector Registry.
--
-- Registry content is PLATFORM reference data (no tenant column): a connector identity, its immutable contract
-- versions, the mapping versions each carries, and the append-only evidence of how it got there.
--
-- What the database guarantees on its own, whatever the application does:
--   * only the platform scope can write any of it; a tenant reads only PUBLISHED / SUPERSEDED / RETIRED contracts
--     (RLS, FORCEd);
--   * a connector id is never reused and never deleted;
--   * a contract version is edited only while DRAFT; from VALIDATED onward its definition (semver, dates, content,
--     references, hash) cannot change, and nothing is ever deleted;
--   * status only moves along DRAFT > VALIDATED > REVIEW > PUBLISHED > SUPERSEDED > RETIRED (or BLOCKED);
--   * PUBLISHED needs all six gates PASSED on the content hash that was validated, and a publisher who is not the
--     author; at most one version per connector is PUBLISHED, and published semantic versions only ever increase;
--   * a mapping version's specification is fixed once its contract leaves DRAFT;
--   * gate attestations, validation runs and transitions are append-only; a human gate is attested by someone other
--     than the author and only during REVIEW.

-- The platform scope the rest of the estate uses for platform-wide reference data.
CREATE OR REPLACE FUNCTION xic_is_platform() RETURNS boolean AS $$
    SELECT NULLIF(current_setting('app.tenant_id', true), '') = '00000000-0000-0000-0000-00000000f001'
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION xic_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

-- ── connectors ──────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS xic_connectors (
    connector_id    VARCHAR(63)  PRIMARY KEY,
    family          VARCHAR(32)  NOT NULL,
    provider_class  VARCHAR(128) NOT NULL,
    owner           VARCHAR(128) NOT NULL,
    lifecycle       VARCHAR(16)  NOT NULL DEFAULT 'DRAFT',
    supported_until TIMESTAMPTZ,
    version         BIGINT       NOT NULL DEFAULT 1,
    created_by      VARCHAR(255) NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_connectors_id_valid CHECK (connector_id ~ '^[a-z][a-z0-9-]{2,62}$'),
    CONSTRAINT xic_connectors_family_valid CHECK (family IN
        ('BANK_FEED', 'PAYMENT_RAIL', 'TAX_AUTHORITY', 'EINVOICE_NETWORK', 'PAYROLL', 'HRIS', 'BENEFITS',
         'ESIGNATURE', 'ERP', 'CRM', 'REGULATOR', 'FILE_TRANSFER', 'API')),
    CONSTRAINT xic_connectors_lifecycle_valid CHECK (lifecycle IN ('DRAFT', 'REVIEW', 'PUBLISHED', 'DEPRECATED', 'RETIRED')),
    CONSTRAINT xic_connectors_version_positive CHECK (version >= 1),
    CONSTRAINT xic_connectors_deprecated_dated CHECK (lifecycle <> 'DEPRECATED' OR supported_until IS NOT NULL),
    CONSTRAINT xic_connectors_text_present CHECK (btrim(provider_class) <> '' AND btrim(owner) <> '')
);

CREATE OR REPLACE FUNCTION xic_connectors_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a connector identity is never deleted or reused' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.connector_id <> OLD.connector_id OR NEW.family <> OLD.family OR NEW.created_by <> OLD.created_by
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'connector identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'connector version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.lifecycle <> NEW.lifecycle AND NOT (
           (OLD.lifecycle = 'DRAFT'      AND NEW.lifecycle IN ('REVIEW', 'PUBLISHED'))
        OR (OLD.lifecycle = 'REVIEW'     AND NEW.lifecycle = 'PUBLISHED')
        OR (OLD.lifecycle = 'PUBLISHED'  AND NEW.lifecycle IN ('DEPRECATED', 'RETIRED'))
        OR (OLD.lifecycle = 'DEPRECATED' AND NEW.lifecycle = 'RETIRED')) THEN
        RAISE EXCEPTION 'connector lifecycle cannot go from % to %', OLD.lifecycle, NEW.lifecycle
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_connectors_guard_trg ON xic_connectors;
CREATE TRIGGER xic_connectors_guard_trg BEFORE UPDATE OR DELETE ON xic_connectors
    FOR EACH ROW EXECUTE FUNCTION xic_connectors_guard();

-- ── contract versions ───────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS xic_contract_versions (
    contract_version_id UUID         PRIMARY KEY,
    connector_id        VARCHAR(63)  NOT NULL REFERENCES xic_connectors(connector_id),
    semver              VARCHAR(32)  NOT NULL,
    semver_major        INTEGER      GENERATED ALWAYS AS (split_part(semver, '.', 1)::int) STORED,
    semver_minor        INTEGER      GENERATED ALWAYS AS (split_part(semver, '.', 2)::int) STORED,
    semver_patch        INTEGER      GENERATED ALWAYS AS (split_part(semver, '.', 3)::int) STORED,
    status              VARCHAR(16)  NOT NULL DEFAULT 'DRAFT',
    effective_from      TIMESTAMPTZ  NOT NULL,
    supported_until     TIMESTAMPTZ,
    migration_plan_ref  TEXT         NOT NULL DEFAULT '',
    replay_analysis_ref TEXT         NOT NULL DEFAULT '',
    rollback_ref        TEXT         NOT NULL DEFAULT '',
    content             JSONB        NOT NULL,
    content_hash        VARCHAR(64)  NOT NULL DEFAULT '',
    baseline_version_id UUID         REFERENCES xic_contract_versions(contract_version_id),
    compat_class        VARCHAR(32)  NOT NULL DEFAULT '',
    compat_report       JSONB,
    blocked_reason      VARCHAR(255) NOT NULL DEFAULT '',
    row_version         BIGINT       NOT NULL DEFAULT 1,
    created_by          VARCHAR(255) NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    validated_at        TIMESTAMPTZ,
    published_by        VARCHAR(255) NOT NULL DEFAULT '',
    published_at        TIMESTAMPTZ,
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_cv_unique_semver UNIQUE (connector_id, semver),
    CONSTRAINT xic_cv_semver_valid CHECK (semver ~ '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'),
    CONSTRAINT xic_cv_status_valid CHECK (status IN
        ('DRAFT', 'VALIDATED', 'REVIEW', 'PUBLISHED', 'SUPERSEDED', 'RETIRED', 'BLOCKED')),
    CONSTRAINT xic_cv_compat_valid CHECK (compat_class IN
        ('', 'NONE', 'POTENTIALLY_COMPATIBLE', 'LIFECYCLE', 'MATERIAL_SECURITY', 'MATERIAL_BUSINESS', 'MATERIAL_EGRESS', 'BREAKING')),
    CONSTRAINT xic_cv_row_version_positive CHECK (row_version >= 1),
    CONSTRAINT xic_cv_sunset_after_effective CHECK (supported_until IS NULL OR supported_until > effective_from),
    CONSTRAINT xic_cv_content_object CHECK (jsonb_typeof(content) = 'object'),
    -- a version past DRAFT carries the hash of exactly what was validated and a rollback path
    CONSTRAINT xic_cv_validated_is_pinned CHECK (
        status IN ('DRAFT', 'BLOCKED')
        OR (content_hash ~ '^[0-9a-f]{64}$' AND validated_at IS NOT NULL AND rollback_ref <> '' AND compat_class <> '')),
    -- a published version was published by someone other than its author
    CONSTRAINT xic_cv_published_by_other CHECK (
        status NOT IN ('PUBLISHED', 'SUPERSEDED', 'RETIRED')
        OR (published_by <> '' AND published_by <> created_by AND published_at IS NOT NULL)),
    CONSTRAINT xic_cv_blocked_reasoned CHECK (status <> 'BLOCKED' OR blocked_reason <> '')
);

-- One current contract per connector.
CREATE UNIQUE INDEX IF NOT EXISTS xic_cv_one_published ON xic_contract_versions (connector_id) WHERE status = 'PUBLISHED';
CREATE INDEX IF NOT EXISTS xic_cv_connector_order ON xic_contract_versions (connector_id, semver_major, semver_minor, semver_patch);

CREATE TABLE IF NOT EXISTS xic_mapping_versions (
    mapping_version_id    UUID        PRIMARY KEY,
    contract_version_id   UUID        NOT NULL REFERENCES xic_contract_versions(contract_version_id),
    mapping_key           VARCHAR(63) NOT NULL,
    version               INTEGER     NOT NULL,
    source_schema         VARCHAR(80) NOT NULL,
    canonical_contract_ref VARCHAR(128) NOT NULL,
    spec                  JSONB       NOT NULL,
    transform_hash        VARCHAR(64) NOT NULL,
    status                VARCHAR(12) NOT NULL DEFAULT 'DRAFT',
    CONSTRAINT xic_mv_unique UNIQUE (contract_version_id, mapping_key, version),
    CONSTRAINT xic_mv_version_positive CHECK (version >= 1),
    CONSTRAINT xic_mv_hash_valid CHECK (transform_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT xic_mv_status_valid CHECK (status IN ('DRAFT', 'PUBLISHED', 'SUPERSEDED', 'RETIRED'))
);
CREATE INDEX IF NOT EXISTS xic_mv_contract ON xic_mapping_versions (contract_version_id);

-- ── append-only evidence ────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS xic_contract_gates (
    gate_id             UUID         PRIMARY KEY,
    ord                 BIGINT       GENERATED ALWAYS AS IDENTITY,
    contract_version_id UUID         NOT NULL REFERENCES xic_contract_versions(contract_version_id),
    gate_key            VARCHAR(24)  NOT NULL,
    outcome             VARCHAR(8)   NOT NULL,
    evidence_ref        TEXT         NOT NULL DEFAULT '',
    detail              JSONB,
    attested_by         VARCHAR(255) NOT NULL,
    at                  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_gates_key_valid CHECK (gate_key IN
        ('SCHEMA_COMPAT', 'SECURITY', 'PRIVACY', 'NEGATIVE_TESTS', 'DOMAIN_ACCEPTANCE', 'ROLLBACK')),
    CONSTRAINT xic_gates_outcome_valid CHECK (outcome IN ('PASSED', 'FAILED')),
    CONSTRAINT xic_gates_evidenced CHECK (evidence_ref <> '')
);
CREATE INDEX IF NOT EXISTS xic_gates_version ON xic_contract_gates (contract_version_id, gate_key, ord);

CREATE TABLE IF NOT EXISTS xic_validation_runs (
    run_id              UUID         PRIMARY KEY,
    ord                 BIGINT       GENERATED ALWAYS AS IDENTITY,
    contract_version_id UUID         NOT NULL REFERENCES xic_contract_versions(contract_version_id),
    outcome             VARCHAR(8)   NOT NULL,
    content_hash        VARCHAR(64)  NOT NULL,
    issues              JSONB        NOT NULL DEFAULT '[]',
    report              JSONB,
    run_by              VARCHAR(255) NOT NULL,
    at                  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_runs_outcome_valid CHECK (outcome IN ('PASSED', 'FAILED')),
    -- a run that passed found nothing wrong
    CONSTRAINT xic_runs_pass_is_clean CHECK (outcome <> 'PASSED' OR jsonb_array_length(issues) = 0)
);
CREATE INDEX IF NOT EXISTS xic_runs_version ON xic_validation_runs (contract_version_id, ord);

CREATE TABLE IF NOT EXISTS xic_contract_transitions (
    ord                 BIGINT       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    contract_version_id UUID         NOT NULL REFERENCES xic_contract_versions(contract_version_id),
    from_status         VARCHAR(16)  NOT NULL,
    to_status           VARCHAR(16)  NOT NULL,
    actor               VARCHAR(255) NOT NULL,
    reason              VARCHAR(255) NOT NULL DEFAULT '',
    at                  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_transitions_move CHECK (from_status <> to_status)
);
CREATE INDEX IF NOT EXISTS xic_transitions_version ON xic_contract_transitions (contract_version_id, ord);

CREATE TABLE IF NOT EXISTS xic_idempotency (
    scope        VARCHAR(24)  NOT NULL,
    idem_key     VARCHAR(128) NOT NULL,
    request_hash VARCHAR(64)  NOT NULL,
    resource_id  VARCHAR(80)  NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, idem_key)
);

DROP TRIGGER IF EXISTS xic_gates_no_change ON xic_contract_gates;
CREATE TRIGGER xic_gates_no_change BEFORE UPDATE OR DELETE ON xic_contract_gates
    FOR EACH ROW EXECUTE FUNCTION xic_append_only();
DROP TRIGGER IF EXISTS xic_runs_no_change ON xic_validation_runs;
CREATE TRIGGER xic_runs_no_change BEFORE UPDATE OR DELETE ON xic_validation_runs
    FOR EACH ROW EXECUTE FUNCTION xic_append_only();
DROP TRIGGER IF EXISTS xic_transitions_no_change ON xic_contract_transitions;
CREATE TRIGGER xic_transitions_no_change BEFORE UPDATE OR DELETE ON xic_contract_transitions
    FOR EACH ROW EXECUTE FUNCTION xic_append_only();
DROP TRIGGER IF EXISTS xic_idem_no_change ON xic_idempotency;
CREATE TRIGGER xic_idem_no_change BEFORE UPDATE OR DELETE ON xic_idempotency
    FOR EACH ROW EXECUTE FUNCTION xic_append_only();

-- A gate is attested at the right time, by the right party.
CREATE OR REPLACE FUNCTION xic_gate_guard() RETURNS trigger AS $$
DECLARE
    v_status TEXT;
    v_author TEXT;
BEGIN
    SELECT status, created_by INTO v_status, v_author FROM xic_contract_versions WHERE contract_version_id = NEW.contract_version_id;
    IF NEW.gate_key = 'SCHEMA_COMPAT' THEN
        IF NEW.attested_by <> 'system:xic-registry' THEN
            RAISE EXCEPTION 'the schema/compatibility gate is attested by the registry itself' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF v_status <> 'DRAFT' THEN
            RAISE EXCEPTION 'validation happens while a version is a draft' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.detail IS NULL OR NEW.detail->>'content_hash' IS NULL THEN
            RAISE EXCEPTION 'the schema/compatibility attestation names the content hash it covers' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSE
        IF NEW.attested_by LIKE 'system:%' THEN
            RAISE EXCEPTION 'a human gate cannot be attested by the system' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.attested_by = v_author THEN
            RAISE EXCEPTION 'a gate must be attested by someone other than the author' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF v_status <> 'REVIEW' THEN
            RAISE EXCEPTION 'gates are attested during review' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_gate_guard_trg ON xic_contract_gates;
CREATE TRIGGER xic_gate_guard_trg BEFORE INSERT ON xic_contract_gates
    FOR EACH ROW EXECUTE FUNCTION xic_gate_guard();

-- The contract version state machine and its immutability.
CREATE OR REPLACE FUNCTION xic_cv_guard() RETURNS trigger AS $$
DECLARE
    g RECORD;
    n INT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a contract version is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.contract_version_id <> OLD.contract_version_id OR NEW.connector_id <> OLD.connector_id
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'contract version identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'contract version row_version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- what a version IS cannot change once it leaves DRAFT (I-03)
    IF OLD.status <> 'DRAFT' AND (NEW.semver <> OLD.semver OR NEW.effective_from <> OLD.effective_from
        OR NEW.supported_until IS DISTINCT FROM OLD.supported_until OR NEW.migration_plan_ref <> OLD.migration_plan_ref
        OR NEW.replay_analysis_ref <> OLD.replay_analysis_ref OR NEW.rollback_ref <> OLD.rollback_ref
        OR NEW.content <> OLD.content) THEN
        RAISE EXCEPTION 'a contract version past DRAFT is immutable; create a new version' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- what validation concluded is fixed once recorded
    IF OLD.status NOT IN ('DRAFT') AND (NEW.content_hash <> OLD.content_hash OR NEW.compat_class <> OLD.compat_class
        OR NEW.compat_report IS DISTINCT FROM OLD.compat_report OR NEW.baseline_version_id IS DISTINCT FROM OLD.baseline_version_id
        OR NEW.validated_at IS DISTINCT FROM OLD.validated_at) THEN
        RAISE EXCEPTION 'validation results are fixed once recorded' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.published_at IS NOT NULL AND (NEW.published_by <> OLD.published_by OR NEW.published_at IS DISTINCT FROM OLD.published_at) THEN
        RAISE EXCEPTION 'publication facts are fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.status <> NEW.status THEN
        IF NOT (
               (OLD.status = 'DRAFT'      AND NEW.status = 'VALIDATED')
            OR (OLD.status = 'VALIDATED'  AND NEW.status IN ('REVIEW', 'BLOCKED'))
            OR (OLD.status = 'REVIEW'     AND NEW.status IN ('PUBLISHED', 'BLOCKED'))
            OR (OLD.status = 'PUBLISHED'  AND NEW.status IN ('SUPERSEDED', 'RETIRED'))
            OR (OLD.status = 'SUPERSEDED' AND NEW.status = 'RETIRED')) THEN
            RAISE EXCEPTION 'contract version status cannot go from % to %', OLD.status, NEW.status
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;

    IF NEW.status = 'PUBLISHED' AND OLD.status <> 'PUBLISHED' THEN
        -- every gate passed, the latest attestation of each, on the content that was validated
        n := 0;
        FOR g IN SELECT DISTINCT ON (gate_key) gate_key, outcome, detail
                   FROM xic_contract_gates WHERE contract_version_id = NEW.contract_version_id
                  ORDER BY gate_key, ord DESC LOOP
            IF g.outcome <> 'PASSED' THEN
                RAISE EXCEPTION 'gate % has not passed', g.gate_key USING ERRCODE = 'integrity_constraint_violation';
            END IF;
            IF g.gate_key = 'SCHEMA_COMPAT' AND g.detail->>'content_hash' IS DISTINCT FROM NEW.content_hash THEN
                RAISE EXCEPTION 'validation covered different content than is being published' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
            n := n + 1;
        END LOOP;
        IF n <> 6 THEN
            RAISE EXCEPTION 'publication needs all six gates passed' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        -- published versions only ever increase
        IF EXISTS (SELECT 1 FROM xic_contract_versions o
                    WHERE o.connector_id = NEW.connector_id AND o.contract_version_id <> NEW.contract_version_id
                      AND o.status IN ('PUBLISHED', 'SUPERSEDED', 'RETIRED')
                      AND (o.semver_major, o.semver_minor, o.semver_patch) >=
                          (split_part(NEW.semver, '.', 1)::int, split_part(NEW.semver, '.', 2)::int, split_part(NEW.semver, '.', 3)::int)) THEN
            RAISE EXCEPTION 'a published version must be greater than every earlier published version' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_cv_guard_trg ON xic_contract_versions;
CREATE TRIGGER xic_cv_guard_trg BEFORE UPDATE OR DELETE ON xic_contract_versions
    FOR EACH ROW EXECUTE FUNCTION xic_cv_guard();

-- Mapping versions follow their contract.
CREATE OR REPLACE FUNCTION xic_mv_guard() RETURNS trigger AS $$
DECLARE
    p_status TEXT;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT status INTO p_status FROM xic_contract_versions WHERE contract_version_id = NEW.contract_version_id;
        IF p_status <> 'DRAFT' OR NEW.status <> 'DRAFT' THEN
            RAISE EXCEPTION 'mapping versions are added to a draft contract only' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    SELECT status INTO p_status FROM xic_contract_versions WHERE contract_version_id = OLD.contract_version_id;
    IF TG_OP = 'DELETE' THEN
        IF p_status <> 'DRAFT' THEN
            RAISE EXCEPTION 'a published mapping version is never deleted' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.mapping_version_id <> OLD.mapping_version_id OR NEW.contract_version_id <> OLD.contract_version_id
       OR NEW.mapping_key <> OLD.mapping_key OR NEW.version <> OLD.version OR NEW.source_schema <> OLD.source_schema
       OR NEW.canonical_contract_ref <> OLD.canonical_contract_ref OR NEW.spec <> OLD.spec OR NEW.transform_hash <> OLD.transform_hash THEN
        RAISE EXCEPTION 'a mapping version is immutable; publish a new version' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> NEW.status AND NOT (
           (OLD.status = 'DRAFT'      AND NEW.status = 'PUBLISHED')
        OR (OLD.status = 'PUBLISHED'  AND NEW.status IN ('SUPERSEDED', 'RETIRED'))
        OR (OLD.status = 'SUPERSEDED' AND NEW.status = 'RETIRED')) THEN
        RAISE EXCEPTION 'mapping status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_mv_guard_trg ON xic_mapping_versions;
CREATE TRIGGER xic_mv_guard_trg BEFORE INSERT OR UPDATE OR DELETE ON xic_mapping_versions
    FOR EACH ROW EXECUTE FUNCTION xic_mv_guard();

-- ── row-level security ──────────────────────────────────────────────────────
-- Everything here is platform data: the platform scope reads and writes it, a tenant reads only what is
-- published. FORCE'd, like every other table in this service.

ALTER TABLE xic_connectors ENABLE ROW LEVEL SECURITY;
ALTER TABLE xic_connectors FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS xic_connectors_read ON xic_connectors;
CREATE POLICY xic_connectors_read ON xic_connectors FOR SELECT
    USING (xic_is_platform() OR lifecycle IN ('PUBLISHED', 'DEPRECATED', 'RETIRED'));
DROP POLICY IF EXISTS xic_connectors_write ON xic_connectors;
CREATE POLICY xic_connectors_write ON xic_connectors FOR INSERT WITH CHECK (xic_is_platform());
DROP POLICY IF EXISTS xic_connectors_update ON xic_connectors;
CREATE POLICY xic_connectors_update ON xic_connectors FOR UPDATE USING (xic_is_platform()) WITH CHECK (xic_is_platform());

ALTER TABLE xic_contract_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE xic_contract_versions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS xic_cv_read ON xic_contract_versions;
CREATE POLICY xic_cv_read ON xic_contract_versions FOR SELECT
    USING (xic_is_platform() OR status IN ('PUBLISHED', 'SUPERSEDED', 'RETIRED'));
DROP POLICY IF EXISTS xic_cv_insert ON xic_contract_versions;
CREATE POLICY xic_cv_insert ON xic_contract_versions FOR INSERT WITH CHECK (xic_is_platform());
DROP POLICY IF EXISTS xic_cv_update ON xic_contract_versions;
CREATE POLICY xic_cv_update ON xic_contract_versions FOR UPDATE USING (xic_is_platform()) WITH CHECK (xic_is_platform());

-- Children are visible exactly when their contract version is (the subquery runs under that table's own policy).
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['xic_mapping_versions', 'xic_contract_gates', 'xic_validation_runs', 'xic_contract_transitions']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_read', t);
        EXECUTE format($p$CREATE POLICY %I ON %I FOR SELECT USING (xic_is_platform() OR EXISTS
            (SELECT 1 FROM xic_contract_versions v WHERE v.contract_version_id = %I.contract_version_id))$p$, t || '_read', t, t);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_insert', t);
        EXECUTE format('CREATE POLICY %I ON %I FOR INSERT WITH CHECK (xic_is_platform())', t || '_insert', t);
    END LOOP;
END $$;
-- mapping versions are also updated (status) and deleted (replacing a draft's mappings)
DROP POLICY IF EXISTS xic_mapping_versions_update ON xic_mapping_versions;
CREATE POLICY xic_mapping_versions_update ON xic_mapping_versions FOR UPDATE USING (xic_is_platform()) WITH CHECK (xic_is_platform());
DROP POLICY IF EXISTS xic_mapping_versions_delete ON xic_mapping_versions;
CREATE POLICY xic_mapping_versions_delete ON xic_mapping_versions FOR DELETE USING (xic_is_platform());

ALTER TABLE xic_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE xic_idempotency FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS xic_idem_platform ON xic_idempotency;
CREATE POLICY xic_idem_platform ON xic_idempotency FOR ALL USING (xic_is_platform()) WITH CHECK (xic_is_platform());
