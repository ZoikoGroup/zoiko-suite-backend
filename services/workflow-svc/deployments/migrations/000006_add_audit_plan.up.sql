-- AUD-02 Planning & Risk Assessment lives beside AUD-01 Engagement in this
-- same service — MarkFieldworkComplete/MarkReportReady need to check plan
-- approval and risk coverage synchronously, in the same database, not via
-- a cross-service call that would turn a correctness gate into a race.

-- scope_version is bumped by AUD-01's own (forthcoming) AmendScope command
-- and snapshotted onto a plan at ApprovePlan time (scope_version_at_approval
-- below) — the mechanism behind "scope/framework changes invalidate
-- dependent approvals."
ALTER TABLE audit_engagements ADD COLUMN scope_version INTEGER NOT NULL DEFAULT 1;

CREATE TABLE audit_plans (
    plan_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    engagement_id UUID NOT NULL REFERENCES audit_engagements(engagement_id),
    tenant_id UUID NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'DRAFT',
    scope_version_at_approval INTEGER,
    created_by_principal_id TEXT NOT NULL,
    approved_by_principal_id TEXT,
    correlation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_from TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_to TIMESTAMPTZ,
    CONSTRAINT audit_plan_status_valid CHECK (status IN ('DRAFT','PREPARED','REVIEWED','APPROVED')),
    CONSTRAINT audit_plan_creator_present CHECK (created_by_principal_id <> ''),
    CONSTRAINT audit_plan_effective_range_valid CHECK (effective_to IS NULL OR effective_to > effective_from)
);
-- One live plan per engagement at a time.
CREATE UNIQUE INDEX audit_plan_live_per_engagement ON audit_plans (engagement_id) WHERE effective_to IS NULL;
CREATE UNIQUE INDEX audit_plan_create_idempotency_unique ON audit_plans (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';

CREATE TABLE audit_plan_transitions (
    transition_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id UUID NOT NULL REFERENCES audit_plans(plan_id),
    tenant_id UUID NOT NULL,
    from_status TEXT NOT NULL,
    to_status TEXT NOT NULL,
    actor_principal_id TEXT NOT NULL,
    correlation_id TEXT,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT audit_plan_transition_actor_present CHECK (actor_principal_id <> '')
);
CREATE UNIQUE INDEX audit_plan_transition_idempotency_unique ON audit_plan_transitions (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';
CREATE INDEX audit_plan_transitions_plan ON audit_plan_transitions (plan_id, occurred_at);

CREATE TABLE materiality_records (
    materiality_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id UUID NOT NULL REFERENCES audit_plans(plan_id),
    tenant_id UUID NOT NULL,
    overall_materiality NUMERIC(18,2) NOT NULL,
    performance_materiality NUMERIC(18,2) NOT NULL,
    clearly_trivial_threshold NUMERIC(18,2),
    rationale TEXT NOT NULL,
    created_by_principal_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_from TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_to TIMESTAMPTZ,
    CONSTRAINT materiality_rationale_present CHECK (rationale <> ''),
    CONSTRAINT materiality_effective_range_valid CHECK (effective_to IS NULL OR effective_to > effective_from)
);
CREATE UNIQUE INDEX materiality_record_live_per_plan ON materiality_records (plan_id) WHERE effective_to IS NULL;

CREATE TABLE risk_assessments (
    risk_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id UUID NOT NULL REFERENCES audit_plans(plan_id),
    engagement_id UUID NOT NULL REFERENCES audit_engagements(engagement_id),
    tenant_id UUID NOT NULL,
    description TEXT NOT NULL,
    risk_level TEXT NOT NULL,
    is_significant BOOLEAN NOT NULL DEFAULT false,
    status TEXT NOT NULL DEFAULT 'IDENTIFIED',
    coverage_status TEXT NOT NULL DEFAULT 'PENDING',
    assessed_by_principal_id TEXT,
    assessed_at TIMESTAMPTZ,
    created_by_principal_id TEXT NOT NULL,
    correlation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_from TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_to TIMESTAMPTZ,
    CONSTRAINT risk_description_present CHECK (description <> ''),
    CONSTRAINT risk_level_valid CHECK (risk_level IN ('LOW','MODERATE','HIGH')),
    CONSTRAINT risk_status_valid CHECK (status IN ('IDENTIFIED','ASSESSED')),
    CONSTRAINT risk_coverage_valid CHECK (coverage_status IN ('PENDING','COVERED','PENDING_REASSESSMENT')),
    -- "Significant-risk classification requires authorized human": no row
    -- can carry is_significant=true without a real actor attributed.
    CONSTRAINT risk_significant_requires_actor CHECK (is_significant = false OR assessed_by_principal_id IS NOT NULL)
);
CREATE UNIQUE INDEX risk_assessment_create_idempotency_unique ON risk_assessments (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';
CREATE INDEX risk_assessments_plan ON risk_assessments (plan_id) WHERE effective_to IS NULL;
CREATE INDEX risk_assessments_engagement_high_uncovered ON risk_assessments (engagement_id) WHERE effective_to IS NULL AND risk_level = 'HIGH' AND coverage_status <> 'COVERED';

CREATE TABLE assertion_links (
    assertion_link_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    risk_id UUID NOT NULL REFERENCES risk_assessments(risk_id),
    tenant_id UUID NOT NULL,
    assertion_code TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT assertion_code_present CHECK (assertion_code <> '')
);

CREATE TABLE planned_procedures (
    procedure_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    risk_id UUID NOT NULL REFERENCES risk_assessments(risk_id),
    tenant_id UUID NOT NULL,
    description TEXT NOT NULL,
    designed_by_principal_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT planned_procedure_description_present CHECK (description <> '')
);

ALTER TABLE audit_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_plans FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_plans_tenant_isolation ON audit_plans FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE audit_plan_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_plan_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_plan_transitions_tenant_isolation ON audit_plan_transitions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE materiality_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE materiality_records FORCE ROW LEVEL SECURITY;
CREATE POLICY materiality_records_tenant_isolation ON materiality_records FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE risk_assessments ENABLE ROW LEVEL SECURITY;
ALTER TABLE risk_assessments FORCE ROW LEVEL SECURITY;
CREATE POLICY risk_assessments_tenant_isolation ON risk_assessments FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE assertion_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE assertion_links FORCE ROW LEVEL SECURITY;
CREATE POLICY assertion_links_tenant_isolation ON assertion_links FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE planned_procedures ENABLE ROW LEVEL SECURITY;
ALTER TABLE planned_procedures FORCE ROW LEVEL SECURITY;
CREATE POLICY planned_procedures_tenant_isolation ON planned_procedures FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Append-only history, mirroring 000005's own audit_engagement_transitions trigger.
CREATE OR REPLACE FUNCTION reject_audit_plan_transition_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit plan transitions are append-only';
END;
$$;
CREATE TRIGGER trg_reject_audit_plan_transition_mutation
    BEFORE UPDATE OR DELETE ON audit_plan_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_audit_plan_transition_mutation();

CREATE OR REPLACE FUNCTION reject_assertion_link_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'assertion links are append-only';
END;
$$;
CREATE TRIGGER trg_reject_assertion_link_mutation
    BEFORE UPDATE OR DELETE ON assertion_links
    FOR EACH ROW EXECUTE FUNCTION reject_assertion_link_mutation();

CREATE OR REPLACE FUNCTION reject_planned_procedure_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'planned procedures are append-only';
END;
$$;
CREATE TRIGGER trg_reject_planned_procedure_mutation
    BEFORE UPDATE OR DELETE ON planned_procedures
    FOR EACH ROW EXECUTE FUNCTION reject_planned_procedure_mutation();

-- A materiality record's own economic fields never change once written —
-- only end-dating it (to make way for a new version, in the same
-- transaction that inserts the replacement) is permitted. This is the
-- "materiality inputs SHALL be explicitly versioned and attributable"
-- control at the DB layer, not just application discipline.
CREATE OR REPLACE FUNCTION reject_materiality_record_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'materiality records are never deleted';
    END IF;
    IF OLD.overall_materiality IS DISTINCT FROM NEW.overall_materiality
        OR OLD.performance_materiality IS DISTINCT FROM NEW.performance_materiality
        OR OLD.clearly_trivial_threshold IS DISTINCT FROM NEW.clearly_trivial_threshold
        OR OLD.rationale IS DISTINCT FROM NEW.rationale
        OR OLD.plan_id IS DISTINCT FROM NEW.plan_id THEN
        RAISE EXCEPTION 'materiality record economic fields are immutable — insert a new version instead';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_reject_materiality_record_mutation
    BEFORE UPDATE OR DELETE ON materiality_records
    FOR EACH ROW EXECUTE FUNCTION reject_materiality_record_mutation();
