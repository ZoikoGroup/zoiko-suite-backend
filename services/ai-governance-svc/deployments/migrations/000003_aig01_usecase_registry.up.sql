-- 000003_aig01_usecase_registry.up.sql
-- ZS-SVC-X-001 §4 — AIG-01: AI Use-Case, Risk & Impact Registry.
--
-- Additive to the doc7-based tables in 000001/000002, which are
-- untouched. No AI capability reaches production until its business
-- use, affected outcome and control class are explicitly registered
-- and approved (§4 SECTION CONTROL) — this is that registry.

-- ai_use_cases: §4.2's registration contract + §4.3's lifecycle.
-- tenant_id is UUID, matching this service's existing convention
-- (ai_runs/automation_policies/automation_actions), not the prefixed
-- TEXT-ID convention used elsewhere in this platform.
CREATE TABLE ai_use_cases (
    use_case_id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    domain                   VARCHAR(128) NOT NULL,
    purpose                  TEXT NOT NULL,
    outcome_type              VARCHAR(128) NOT NULL,
    operational_class            VARCHAR(8) NOT NULL
        CHECK (operational_class IN ('A0','A1','A2','A3','A4')),
    legal_classification_ref        VARCHAR(255),
    owner_principal_id                 VARCHAR(255) NOT NULL,
    business_outcome                      TEXT NOT NULL,
    affected_decisions                       JSONB NOT NULL DEFAULT '[]'::jsonb,
    data_profile                                JSONB NOT NULL DEFAULT '{}'::jsonb,
    automation_level                               VARCHAR(32) NOT NULL
        CHECK (automation_level IN ('DRAFT','RECOMMENDATION','EXTRACTION','CLASSIFICATION','RANKING','AUTONOMOUS_TOOL_PLANNING','PROHIBITED')),
    human_role                                        JSONB NOT NULL DEFAULT '{}'::jsonb,
    fallback                                             TEXT NOT NULL DEFAULT '',
    success_measures                                        TEXT NOT NULL DEFAULT '',
    prohibited_boundary                                        TEXT NOT NULL DEFAULT '',
    retirement_criteria                                           TEXT NOT NULL DEFAULT '',
    lifecycle_state                                                  VARCHAR(16) NOT NULL DEFAULT 'DRAFT'
        CHECK (lifecycle_state IN ('DRAFT','ASSESSING','APPROVED','ACTIVE','LIMITED','SUSPENDED','REJECTED','RETIRED')),
    -- §4.4: "Create draft; idempotent client_request_id."
    client_request_id                                                    VARCHAR(255),
    created_at                                                          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id                                                VARCHAR(255) NOT NULL,
    updated_at                                                                TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_ai_use_cases_tenant ON ai_use_cases (tenant_id, lifecycle_state);
CREATE UNIQUE INDEX idx_ai_use_cases_idempotent_create ON ai_use_cases (tenant_id, client_request_id)
    WHERE client_request_id IS NOT NULL;

-- ai_impact_assessments: §4's "preserve historical versions so later
-- reclassification cannot falsify prior controls" — versioned,
-- immutable once decided (approved or rejected).
CREATE TABLE ai_impact_assessments (
    assessment_id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    use_case_id              UUID NOT NULL REFERENCES ai_use_cases(use_case_id),
    tenant_id                UUID NOT NULL,
    version                  INT NOT NULL,
    affected_groups              JSONB NOT NULL DEFAULT '[]'::jsonb,
    rights_impact                   TEXT NOT NULL DEFAULT '',
    financial_impact                   TEXT NOT NULL DEFAULT '',
    employment_impact                     TEXT NOT NULL DEFAULT '',
    mitigations                              TEXT NOT NULL DEFAULT '',
    approvers                                   JSONB NOT NULL DEFAULT '[]'::jsonb,
    decision                                       VARCHAR(16) NOT NULL DEFAULT 'PENDING'
        CHECK (decision IN ('PENDING','APPROVED','REJECTED')),
    decided_by_principal_id                           VARCHAR(255),
    decision_reason                                      TEXT,
    decided_at                                              TIMESTAMP WITH TIME ZONE,
    expires_at                                                 TIMESTAMP WITH TIME ZONE,
    created_at                                                    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id                                          VARCHAR(255) NOT NULL,
    UNIQUE (use_case_id, version)
);

CREATE INDEX idx_ai_impact_assessments_use_case ON ai_impact_assessments (use_case_id, version DESC);

-- ── Row Level Security (tenant-scoped, same convention as 000002) ──────────

ALTER TABLE ai_use_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_use_cases FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON ai_use_cases
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE ai_impact_assessments ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_impact_assessments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON ai_impact_assessments
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ── Lifecycle triggers ───────────────────────────────────────────────────────

-- ai_impact_assessments: immutable once decided — PENDING rows may only
-- be updated to record their own decision; a decided row never changes
-- again. This is what makes "preserve historical versions so later
-- reclassification cannot falsify prior controls" true structurally.
CREATE OR REPLACE FUNCTION aig01_enforce_assessment_immutability()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ai_impact_assessments rows cannot be deleted';
    END IF;
    IF OLD.decision <> 'PENDING' THEN
        RAISE EXCEPTION 'ai_impact_assessment % is % and immutable', OLD.assessment_id, OLD.decision;
    END IF;
    IF NEW.use_case_id IS DISTINCT FROM OLD.use_case_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.version IS DISTINCT FROM OLD.version
        OR NEW.affected_groups IS DISTINCT FROM OLD.affected_groups
        OR NEW.rights_impact IS DISTINCT FROM OLD.rights_impact
        OR NEW.financial_impact IS DISTINCT FROM OLD.financial_impact
        OR NEW.employment_impact IS DISTINCT FROM OLD.employment_impact
        OR NEW.mitigations IS DISTINCT FROM OLD.mitigations
        OR NEW.approvers IS DISTINCT FROM OLD.approvers
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
    THEN
        RAISE EXCEPTION 'ai_impact_assessment % may only have its decision fields change', OLD.assessment_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_assessment_immutability
    BEFORE UPDATE OR DELETE ON ai_impact_assessments
    FOR EACH ROW EXECUTE FUNCTION aig01_enforce_assessment_immutability();

-- ai_use_cases: forward-only lifecycle per §4.3's state diagram.
-- REJECTED and RETIRED are fully terminal.
CREATE OR REPLACE FUNCTION aig01_enforce_use_case_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ai_use_cases rows cannot be deleted';
    END IF;
    IF OLD.lifecycle_state IN ('REJECTED', 'RETIRED') THEN
        RAISE EXCEPTION 'ai_use_case % is % and immutable', OLD.use_case_id, OLD.lifecycle_state;
    END IF;
    IF OLD.lifecycle_state <> NEW.lifecycle_state THEN
        CASE OLD.lifecycle_state
            WHEN 'DRAFT' THEN
                IF NEW.lifecycle_state <> 'ASSESSING' THEN
                    RAISE EXCEPTION 'invalid ai_use_case transition from DRAFT to %', NEW.lifecycle_state;
                END IF;
            WHEN 'ASSESSING' THEN
                IF NEW.lifecycle_state NOT IN ('APPROVED', 'REJECTED') THEN
                    RAISE EXCEPTION 'invalid ai_use_case transition from ASSESSING to %', NEW.lifecycle_state;
                END IF;
            WHEN 'APPROVED' THEN
                IF NEW.lifecycle_state NOT IN ('ACTIVE', 'LIMITED', 'ASSESSING') THEN
                    RAISE EXCEPTION 'invalid ai_use_case transition from APPROVED to %', NEW.lifecycle_state;
                END IF;
            WHEN 'ACTIVE' THEN
                IF NEW.lifecycle_state NOT IN ('LIMITED', 'SUSPENDED', 'RETIRED', 'ASSESSING') THEN
                    RAISE EXCEPTION 'invalid ai_use_case transition from ACTIVE to %', NEW.lifecycle_state;
                END IF;
            WHEN 'LIMITED' THEN
                IF NEW.lifecycle_state NOT IN ('ACTIVE', 'SUSPENDED', 'RETIRED', 'ASSESSING') THEN
                    RAISE EXCEPTION 'invalid ai_use_case transition from LIMITED to %', NEW.lifecycle_state;
                END IF;
            WHEN 'SUSPENDED' THEN
                IF NEW.lifecycle_state NOT IN ('ASSESSING', 'RETIRED') THEN
                    RAISE EXCEPTION 'invalid ai_use_case transition from SUSPENDED to %', NEW.lifecycle_state;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown ai_use_case lifecycle_state %', OLD.lifecycle_state;
        END CASE;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.domain IS DISTINCT FROM OLD.domain
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
    THEN
        RAISE EXCEPTION 'ai_use_case % tenant/domain/creation fields are immutable', OLD.use_case_id;
    END IF;
    NEW.updated_at := NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_use_case_lifecycle
    BEFORE UPDATE OR DELETE ON ai_use_cases
    FOR EACH ROW EXECUTE FUNCTION aig01_enforce_use_case_lifecycle();
