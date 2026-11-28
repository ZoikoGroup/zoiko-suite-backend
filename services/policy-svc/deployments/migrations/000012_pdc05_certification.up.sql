-- Migration: 000012_pdc05_certification.up.sql
--
-- V-001 PDC-05: certification test suites, waivers and differential
-- simulation for policy versions.
--
--   policy_test_cases       known inputs and expected results per family
--                           (UNIT / BOUNDARY / REGRESSION), versioned by
--                           retiring and adding, never by editing
--   policy_test_runs        immutable results of running a family's suite
--                           against one candidate version (PASS / FAIL)
--   policy_test_waivers     typed, scoped, time-bounded, maker-checker
--                           approved exceptions for one failed case of one
--                           version; never a pass (PDC-I-25)
--   policy_simulation_runs  candidate vs baseline over the tenant's own
--                           recorded evaluations; writes only here (PDC-I-23)
--   policy_simulation_diffs the decisions whose result would change
--
-- Every table is tenant-owned and under FORCE RLS; a platform family's test
-- cases have no tenant and are visible to all, like the family.

CREATE TABLE policy_test_cases (
    test_case_id            UUID         PRIMARY KEY,
    policy_id               UUID         NOT NULL REFERENCES policies (policy_id),
    tenant_id               UUID,
    test_code               VARCHAR(128) NOT NULL,
    layer                   VARCHAR(16)  NOT NULL CHECK (layer IN ('UNIT', 'BOUNDARY', 'REGRESSION')),
    mandatory               BOOLEAN      NOT NULL DEFAULT TRUE,
    description             TEXT         NOT NULL,
    action_context          JSONB        NOT NULL,
    expected_result         VARCHAR(64)  NOT NULL,
    test_status             VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE' CHECK (test_status IN ('ACTIVE', 'RETIRED')),
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,
    retired_at              TIMESTAMPTZ,
    retired_by_principal_id TEXT
);
CREATE UNIQUE INDEX idx_policy_test_cases_code ON policy_test_cases (policy_id, test_code);
CREATE INDEX idx_policy_test_cases_active ON policy_test_cases (policy_id) WHERE test_status = 'ACTIVE';

-- A case's content never changes; only ACTIVE -> RETIRED, once.
CREATE FUNCTION policy_test_cases_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'policy_test_cases is append-only' USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.test_case_id IS DISTINCT FROM OLD.test_case_id
       OR NEW.policy_id IS DISTINCT FROM OLD.policy_id
       OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.test_code IS DISTINCT FROM OLD.test_code
       OR NEW.layer IS DISTINCT FROM OLD.layer
       OR NEW.mandatory IS DISTINCT FROM OLD.mandatory
       OR NEW.description IS DISTINCT FROM OLD.description
       OR NEW.action_context IS DISTINCT FROM OLD.action_context
       OR NEW.expected_result IS DISTINCT FROM OLD.expected_result
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
       OR OLD.test_status = 'RETIRED' THEN
        RAISE EXCEPTION 'a test case is immutable; retire it and add another' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_policy_test_cases_guard
    BEFORE UPDATE OR DELETE ON policy_test_cases
    FOR EACH ROW EXECUTE FUNCTION policy_test_cases_guard();

CREATE TABLE policy_test_runs (
    test_run_id             UUID         PRIMARY KEY,
    tenant_id               UUID,
    policy_id               UUID         NOT NULL REFERENCES policies (policy_id),
    policy_version_id       UUID         NOT NULL REFERENCES policy_versions (policy_version_id),
    artifact_digest         TEXT         NOT NULL,
    suite_hash              TEXT         NOT NULL,
    evaluator_version       TEXT         NOT NULL,
    run_status              VARCHAR(8)   NOT NULL CHECK (run_status IN ('PASS', 'FAIL')),
    case_count              INTEGER      NOT NULL,
    failed_count            INTEGER      NOT NULL,
    static_findings         JSONB        NOT NULL DEFAULT '[]'::jsonb,
    results                 JSONB        NOT NULL,
    run_by_principal_id     TEXT         NOT NULL,
    run_at                  TIMESTAMPTZ  NOT NULL
);
CREATE INDEX idx_policy_test_runs_version ON policy_test_runs (policy_version_id, run_at DESC);
CREATE TRIGGER trg_policy_test_runs_append_only
    BEFORE UPDATE OR DELETE ON policy_test_runs
    FOR EACH ROW EXECUTE FUNCTION policy_svc_append_only();

CREATE TABLE policy_test_waivers (
    waiver_id               UUID         PRIMARY KEY,
    tenant_id               UUID,
    policy_version_id       UUID         NOT NULL REFERENCES policy_versions (policy_version_id),
    test_case_id            UUID         NOT NULL REFERENCES policy_test_cases (test_case_id),
    reason_code             VARCHAR(64)  NOT NULL,
    justification           TEXT         NOT NULL,
    expires_at              TIMESTAMPTZ  NOT NULL,
    waiver_status           VARCHAR(16)  NOT NULL DEFAULT 'REQUESTED' CHECK (waiver_status IN ('REQUESTED', 'APPROVED', 'REVOKED')),
    requested_by_principal_id TEXT       NOT NULL,
    requested_at            TIMESTAMPTZ  NOT NULL,
    approved_by_principal_id  TEXT,
    approved_at             TIMESTAMPTZ,
    revoked_by_principal_id TEXT,
    revoked_at              TIMESTAMPTZ,
    revocation_reason       TEXT,
    CONSTRAINT waiver_expires_after_request CHECK (expires_at > requested_at),
    CONSTRAINT waiver_maker_is_not_checker CHECK (approved_by_principal_id IS NULL OR approved_by_principal_id <> requested_by_principal_id),
    CONSTRAINT waiver_approval_evidenced CHECK (waiver_status <> 'APPROVED' OR (approved_by_principal_id IS NOT NULL AND approved_at IS NOT NULL))
);
CREATE INDEX idx_policy_test_waivers_version ON policy_test_waivers (policy_version_id);

CREATE TABLE policy_simulation_runs (
    simulation_id           UUID         PRIMARY KEY,
    tenant_id               UUID         NOT NULL,
    policy_type             VARCHAR(64)  NOT NULL,
    candidate_version_id    UUID         NOT NULL REFERENCES policy_versions (policy_version_id),
    baseline_version_id     UUID         REFERENCES policy_versions (policy_version_id),
    sim_status              VARCHAR(16)  NOT NULL CHECK (sim_status IN ('QUEUED', 'RUNNING', 'COMPLETE', 'PARTIAL', 'FAILED')),
    corpus_from             TIMESTAMPTZ  NOT NULL,
    corpus_to               TIMESTAMPTZ  NOT NULL,
    corpus_limit            INTEGER      NOT NULL,
    corpus_size             INTEGER      NOT NULL DEFAULT 0,
    changed_count           INTEGER      NOT NULL DEFAULT 0,
    error_count             INTEGER      NOT NULL DEFAULT 0,
    requested_by_principal_id TEXT       NOT NULL,
    created_at              TIMESTAMPTZ  NOT NULL,
    completed_at            TIMESTAMPTZ
);
CREATE INDEX idx_policy_simulation_runs_candidate ON policy_simulation_runs (candidate_version_id);

CREATE TABLE policy_simulation_diffs (
    simulation_id           UUID         NOT NULL REFERENCES policy_simulation_runs (simulation_id),
    evaluation_id           UUID         NOT NULL,
    tenant_id               UUID         NOT NULL,
    decision_id             TEXT         NOT NULL,
    baseline_result         VARCHAR(64),
    candidate_result        VARCHAR(64),
    candidate_error         TEXT,
    PRIMARY KEY (simulation_id, evaluation_id)
);
CREATE TRIGGER trg_policy_simulation_diffs_append_only
    BEFORE UPDATE OR DELETE ON policy_simulation_diffs
    FOR EACH ROW EXECUTE FUNCTION policy_svc_append_only();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['policy_test_cases', 'policy_test_runs', 'policy_test_waivers'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, t);
    END LOOP;
    FOREACH t IN ARRAY ARRAY['policy_simulation_runs', 'policy_simulation_diffs'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, t);
    END LOOP;
END
$$;
