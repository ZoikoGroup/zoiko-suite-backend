-- 000007_gov05_lifecycle.down.sql
--
-- Refuses while any version is in a state the 000006 code cannot read
-- (REVIEW / APPROVED / SCHEDULED / SUSPENDED): rolling back would silently
-- turn a suspended or unapproved version into something the old binary
-- treats as live or activatable.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM policy_versions
        WHERE version_status IN ('REVIEW', 'APPROVED', 'SCHEDULED', 'SUSPENDED')
    ) THEN
        RAISE EXCEPTION '000007 down refused: versions exist in GOV-05 states the previous schema cannot represent';
    END IF;
END;
$$;

DROP POLICY IF EXISTS tenant_isolation_policy ON attestations;
ALTER TABLE attestations NO FORCE ROW LEVEL SECURITY;
ALTER TABLE attestations DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON control_test_executions;
ALTER TABLE control_test_executions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE control_test_executions DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON control_test_definitions;
ALTER TABLE control_test_definitions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE control_test_definitions DISABLE ROW LEVEL SECURITY;
DROP INDEX IF EXISTS idx_control_test_definitions_code_per_owner;
CREATE UNIQUE INDEX idx_control_test_definitions_code_unique ON control_test_definitions (test_code);
ALTER TABLE attestations             DROP COLUMN IF EXISTS tenant_id;
ALTER TABLE control_test_executions  DROP COLUMN IF EXISTS tenant_id;
ALTER TABLE control_test_definitions DROP COLUMN IF EXISTS tenant_id;

ALTER TABLE outbox ALTER COLUMN payload TYPE JSONB USING convert_from(payload, 'UTF8')::jsonb;
ALTER TABLE outbox
    DROP COLUMN IF EXISTS next_attempt_at,
    DROP COLUMN IF EXISTS causation_id,
    DROP COLUMN IF EXISTS aggregate_version,
    DROP COLUMN IF EXISTS aggregate_id,
    DROP COLUMN IF EXISTS event_id;

DROP TABLE IF EXISTS policy_evidence_outbox;
DROP TABLE IF EXISTS policy_evaluations;

DROP TRIGGER IF EXISTS trg_policy_versions_watermark ON policy_versions;
DROP FUNCTION IF EXISTS bump_policy_resolution_watermark();
DROP TABLE IF EXISTS policy_resolution_watermark;

DROP TABLE IF EXISTS policy_activation_schedule;
DROP TABLE IF EXISTS policy_version_transitions;
DROP FUNCTION IF EXISTS policy_svc_append_only();

DROP TRIGGER IF EXISTS trg_policies_no_truncate ON policies;
DROP TRIGGER IF EXISTS trg_policies_guard ON policies;
DROP FUNCTION IF EXISTS policies_guard();
DROP TRIGGER IF EXISTS trg_policy_versions_no_truncate ON policy_versions;
DROP TRIGGER IF EXISTS trg_policy_versions_guard ON policy_versions;
DROP FUNCTION IF EXISTS policy_versions_guard();
DROP FUNCTION IF EXISTS policy_svc_refuse_truncate();
DROP TRIGGER IF EXISTS trg_policy_versions_set_digest ON policy_versions;
DROP FUNCTION IF EXISTS policy_versions_set_digest();

DROP INDEX IF EXISTS idx_policy_versions_bound;
ALTER TABLE policy_versions DROP CONSTRAINT IF EXISTS chk_policy_versions_window;
ALTER TABLE policy_versions DROP CONSTRAINT IF EXISTS chk_policy_versions_status;
DROP INDEX IF EXISTS idx_policy_versions_one_scheduled_per_scope;
DROP INDEX IF EXISTS idx_policy_versions_request_key;
DROP INDEX IF EXISTS idx_policy_versions_number;
ALTER TABLE policy_versions ALTER COLUMN artifact_digest DROP NOT NULL;
ALTER TABLE policy_versions ALTER COLUMN version_number DROP NOT NULL;
CREATE UNIQUE INDEX idx_policy_versions_dedup ON policy_versions (
    policy_id,
    COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
    COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID),
    effective_from,
    COALESCE(version_number, 0)
);
ALTER TABLE policy_versions
    DROP COLUMN IF EXISTS superseded_by_version_id,
    DROP COLUMN IF EXISTS superseded_at,
    DROP COLUMN IF EXISTS withdrawn_at,
    DROP COLUMN IF EXISTS approval_reference,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS approved_by_principal_id,
    DROP COLUMN IF EXISTS submitted_at,
    DROP COLUMN IF EXISTS submitted_by_principal_id,
    DROP COLUMN IF EXISTS create_request_key,
    DROP COLUMN IF EXISTS row_version;

DROP POLICY IF EXISTS tenant_isolation_policy ON policies;
ALTER TABLE policies NO FORCE ROW LEVEL SECURITY;
ALTER TABLE policies DISABLE ROW LEVEL SECURITY;
DROP INDEX IF EXISTS idx_policies_code_per_owner;
CREATE UNIQUE INDEX idx_policies_code_unique ON policies (policy_code);
