-- Migration: 000008_rule_versions.up.sql
--
-- A11  A definition's rules move into an append-only series of immutable
--      rule versions (financial-control-svc's control_rule_versions
--      pattern). A version is a draft until it is approved by someone other
--      than its creator, and governs runs from its effective date. The
--      definition row keeps only its identity (tenant, legal entity, name);
--      new rows no longer carry rule columns.
--
-- Existing definitions are NOT given a rule version: their rules were never
-- approved, and an approval cannot be invented. Until a version is published
-- for one (based on its current version number) and approved, it cannot start
-- runs. Runs already started keep the rules they pinned.

CREATE TABLE IF NOT EXISTS reconciliation_rule_versions (
    rule_version_id TEXT         PRIMARY KEY
        CHECK (rule_version_id ~ '^rrv_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id       VARCHAR(64)  NOT NULL,
    definition_id   TEXT         NOT NULL REFERENCES reconciliation_definitions(definition_id),
    rule_version    BIGINT       NOT NULL CHECK (rule_version > 0),
    rules           JSONB        NOT NULL CHECK (jsonb_typeof(rules) = 'object'),
    rules_sha256    CHAR(64)     NOT NULL CHECK (rules_sha256 ~ '^[0-9a-f]{64}$'),
    effective_from  DATE         NOT NULL DEFAULT CURRENT_DATE,
    reason          TEXT         NOT NULL,
    created_by      VARCHAR(255) NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    approved_by     VARCHAR(255),
    approved_at     TIMESTAMPTZ,
    -- Maker-checker: the creator never approves their own version.
    CHECK (approved_by IS NULL OR approved_by <> created_by),
    CHECK ((approved_by IS NULL) = (approved_at IS NULL)),
    UNIQUE (definition_id, rule_version)
);
CREATE INDEX IF NOT EXISTS idx_reconciliation_rule_versions_effective
    ON reconciliation_rule_versions (tenant_id, definition_id, effective_from DESC, rule_version DESC)
    WHERE approved_by IS NOT NULL;

ALTER TABLE reconciliation_rule_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_rule_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY reconciliation_rule_versions_tenant_isolation ON reconciliation_rule_versions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- A version is never deleted; only its approval may be recorded, once.
CREATE OR REPLACE FUNCTION guard_rule_version()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation rule versions cannot be deleted';
    END IF;
    IF OLD.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'reconciliation rule version % is approved and immutable', OLD.rule_version_id;
    END IF;
    IF (NEW.rule_version_id, NEW.tenant_id, NEW.definition_id, NEW.rule_version, NEW.rules, NEW.rules_sha256,
        NEW.effective_from, NEW.reason, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.rule_version_id, OLD.tenant_id, OLD.definition_id, OLD.rule_version, OLD.rules, OLD.rules_sha256,
        OLD.effective_from, OLD.reason, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'only the approval of reconciliation rule version % may be recorded', OLD.rule_version_id;
    END IF;
    NEW.approved_at := COALESCE(NEW.approved_at, NOW());
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_guard_rule_version
    BEFORE UPDATE OR DELETE ON reconciliation_rule_versions
    FOR EACH ROW EXECUTE FUNCTION guard_rule_version();

-- ── Definitions: identity only ──────────────────────────────────────────────

ALTER TABLE reconciliation_definitions DROP CONSTRAINT IF EXISTS reconciliation_definitions_sourced_check;
ALTER TABLE reconciliation_definitions DROP CONSTRAINT IF EXISTS reconciliation_definitions_tolerance_policy_check;
ALTER TABLE reconciliation_definitions DROP CONSTRAINT IF EXISTS reconciliation_definitions_governance_check;
ALTER TABLE reconciliation_definitions
    ALTER COLUMN match_key_fields DROP NOT NULL,
    ALTER COLUMN balance_dimensions DROP NOT NULL,
    ALTER COLUMN balance_dimensions DROP DEFAULT;

-- New definitions carry their identity and nothing else; the earlier
-- per-row rule requirements are replaced by this one.
ALTER TABLE reconciliation_definitions
    ADD CONSTRAINT reconciliation_definitions_identity_only_check CHECK (
        legal_entity_id IS NOT NULL
        AND populations IS NULL AND match_key_fields IS NULL AND balance_dimensions IS NULL
        AND amount_scale IS NULL AND tolerance_policy IS NULL AND tolerance_amount_minor_units IS NULL
        AND materiality_amount_minor_units IS NULL AND materiality_policy IS NULL
        AND exception_policy IS NULL AND certification_profile IS NULL
    ) NOT VALID;

-- A definition row is immutable: its rules change only through rule versions.
CREATE OR REPLACE FUNCTION enforce_definition_versioning()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation definitions cannot be deleted';
    END IF;
    RAISE EXCEPTION 'reconciliation definition % is immutable: its legal_entity_id and name are fixed, and its rules change only through rule versions',
        OLD.definition_id;
END;
$$ LANGUAGE plpgsql;

-- ── Runs: pin the rule version's digest ─────────────────────────────────────

ALTER TABLE reconciliation_runs ADD COLUMN rules_sha256 CHAR(64) CHECK (rules_sha256 ~ '^[0-9a-f]{64}$');
ALTER TABLE reconciliation_runs
    ADD CONSTRAINT reconciliation_runs_rules_digest_check CHECK (rules_sha256 IS NOT NULL) NOT VALID;

CREATE OR REPLACE FUNCTION enforce_run_governance_pinned()
RETURNS trigger AS $$
BEGIN
    IF NEW.materiality_policy IS DISTINCT FROM OLD.materiality_policy
        OR NEW.exception_policy IS DISTINCT FROM OLD.exception_policy
        OR NEW.certification_profile IS DISTINCT FROM OLD.certification_profile
        OR NEW.rules_sha256 IS DISTINCT FROM OLD.rules_sha256
    THEN
        RAISE EXCEPTION 'reconciliation run % cannot change its pinned scope or rules', OLD.run_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
