-- AUD-01 Engagement is implemented in workflow-svc as a typed, governed
-- lifecycle module.  Generic workflow instances remain generic and are not
-- repurposed as a bag of audit fields.
CREATE TABLE audit_engagements (
    engagement_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    legal_entity_id UUID NOT NULL,
    engagement_code TEXT NOT NULL,
    engagement_type TEXT NOT NULL,
    reporting_period_start DATE NOT NULL,
    reporting_period_end DATE NOT NULL,
    framework_profile_id TEXT NOT NULL,
    framework_profile_version TEXT NOT NULL,
    methodology_id TEXT NOT NULL,
    methodology_version TEXT NOT NULL,
    responsible_partner_id TEXT NOT NULL,
    scope_summary TEXT NOT NULL,
    -- The submitted document and its immutable version are stored separately.
    -- A free-text evidence reference cannot establish either provenance or the
    -- exact bytes that supported an acceptance decision.
    acceptance_document_id UUID,
    acceptance_document_version INTEGER,
    status TEXT NOT NULL DEFAULT 'PROPOSED',
    created_by_principal_id TEXT NOT NULL,
    correlation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_from TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_to TIMESTAMPTZ,
    CONSTRAINT audit_engagement_period_valid CHECK (reporting_period_end >= reporting_period_start),
    CONSTRAINT audit_engagement_effective_range_valid CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT audit_engagement_status_valid CHECK (status IN ('PROPOSED','ACCEPTANCE_REVIEW','ACCEPTED','ACTIVE','FIELDWORK_COMPLETE','COMPLETION_REVIEW','REPORT_READY','RELEASED','CLOSED','WITHDRAWN')),
    CONSTRAINT audit_engagement_actor_present CHECK (created_by_principal_id <> ''),
    CONSTRAINT audit_engagement_acceptance_evidence_complete CHECK (
        (acceptance_document_id IS NULL AND acceptance_document_version IS NULL)
        OR (acceptance_document_id IS NOT NULL AND acceptance_document_version >= 1)
    )
);

CREATE UNIQUE INDEX audit_engagement_code_unique
    ON audit_engagements (tenant_id, legal_entity_id, engagement_code)
    WHERE effective_to IS NULL;
CREATE UNIQUE INDEX audit_engagement_create_idempotency_unique
    ON audit_engagements (tenant_id, correlation_id)
    WHERE correlation_id IS NOT NULL AND correlation_id <> '';

-- Every material audit state change is an append-only, idempotent fact.
CREATE TABLE audit_engagement_transitions (
    transition_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    engagement_id UUID NOT NULL REFERENCES audit_engagements(engagement_id),
    tenant_id UUID NOT NULL,
    from_status TEXT NOT NULL,
    to_status TEXT NOT NULL,
    actor_principal_id TEXT NOT NULL,
    evidence_document_id UUID,
    evidence_document_version INTEGER,
    correlation_id TEXT,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT audit_engagement_transition_actor_present CHECK (actor_principal_id <> ''),
    CONSTRAINT audit_engagement_transition_evidence_complete CHECK (
        (evidence_document_id IS NULL AND evidence_document_version IS NULL)
        OR (evidence_document_id IS NOT NULL AND evidence_document_version >= 1)
    )
);
CREATE UNIQUE INDEX audit_engagement_transition_idempotency_unique
    ON audit_engagement_transitions (tenant_id, correlation_id)
    WHERE correlation_id IS NOT NULL AND correlation_id <> '';
CREATE INDEX audit_engagement_transitions_engagement ON audit_engagement_transitions (engagement_id, occurred_at);

ALTER TABLE audit_engagements ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_engagements FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_engagements_tenant_isolation ON audit_engagements FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE audit_engagement_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_engagement_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_engagement_transitions_tenant_isolation ON audit_engagement_transitions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE OR REPLACE FUNCTION reject_audit_engagement_transition_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit engagement transitions are append-only';
END;
$$;
CREATE TRIGGER trg_reject_audit_engagement_transition_mutation
    BEFORE UPDATE OR DELETE ON audit_engagement_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_audit_engagement_transition_mutation();
