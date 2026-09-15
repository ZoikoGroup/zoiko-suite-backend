-- AUD-09 Review & Sign-Off builds on the existing generic workflow engine
-- (workflow_instances/workflow_stages/workflow_transitions) rather than
-- parallel machinery: each SignOff drives one real WorkflowInstance
-- (workflow_type='AUDIT_REVIEW_SIGNOFF') through the existing SubmitAction
-- APPROVE path, reusing its SoD enforcement (ErrInitiatorCannotBeApprover /
-- ErrSelfApprovalNotAllowed) for free. These tables add exactly what the
-- generic engine does not have: content-fingerprint binding, mandatory
-- review notes, and invalidation on later protected-content changes.

CREATE TABLE review_scopes (
    review_scope_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    engagement_id UUID NOT NULL REFERENCES audit_engagements(engagement_id),
    tenant_id UUID NOT NULL,
    target_type TEXT NOT NULL,
    target_id UUID NOT NULL,
    -- The principal responsible for the target under review (e.g. a
    -- workpaper's own preparer) — NOT necessarily whoever called
    -- OpenReview. Every fresh sign-off's WorkflowInstance is created with
    -- this as InitiatedBy, so the generic engine's own SoD machinery
    -- applies unchanged.
    initiated_by_principal_id TEXT NOT NULL,
    correlation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT review_scope_target_type_valid CHECK (target_type IN ('WORKPAPER','ENGAGEMENT','FINDING')),
    CONSTRAINT review_scope_initiator_present CHECK (initiated_by_principal_id <> '')
);
CREATE UNIQUE INDEX review_scope_create_idempotency_unique ON review_scopes (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';
CREATE UNIQUE INDEX review_scope_target_unique ON review_scopes (engagement_id, target_type, target_id);

CREATE TABLE review_assignments (
    assignment_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    review_scope_id UUID NOT NULL REFERENCES review_scopes(review_scope_id),
    tenant_id UUID NOT NULL,
    reviewer_principal_id TEXT NOT NULL,
    role TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT review_assignment_reviewer_present CHECK (reviewer_principal_id <> '')
);
CREATE UNIQUE INDEX review_assignment_unique ON review_assignments (review_scope_id, reviewer_principal_id, role);

CREATE TABLE review_notes (
    review_note_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    review_scope_id UUID NOT NULL REFERENCES review_scopes(review_scope_id),
    tenant_id UUID NOT NULL,
    raised_by_principal_id TEXT NOT NULL,
    body TEXT NOT NULL,
    mandatory BOOLEAN NOT NULL DEFAULT true,
    status TEXT NOT NULL DEFAULT 'OPEN',
    response_body TEXT,
    resolved_by_principal_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ,
    CONSTRAINT review_note_body_present CHECK (body <> ''),
    CONSTRAINT review_note_status_valid CHECK (status IN ('OPEN','RESPONDED','RESOLVED'))
);
CREATE INDEX review_notes_scope_unresolved_mandatory ON review_notes (review_scope_id) WHERE mandatory = true AND status <> 'RESOLVED';

-- Review notes are never deleted, and only status/response/resolver
-- fields may change — "review note is deleted rather than resolved" is
-- structurally impossible: no DELETE path exists in the store, and this
-- trigger additionally blocks any attempt to alter body/raised_by.
CREATE OR REPLACE FUNCTION reject_review_note_content_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'review notes are never deleted';
    END IF;
    IF OLD.body IS DISTINCT FROM NEW.body OR OLD.raised_by_principal_id IS DISTINCT FROM NEW.raised_by_principal_id
        OR OLD.review_scope_id IS DISTINCT FROM NEW.review_scope_id THEN
        RAISE EXCEPTION 'review note original content is immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_reject_review_note_content_mutation
    BEFORE UPDATE OR DELETE ON review_notes
    FOR EACH ROW EXECUTE FUNCTION reject_review_note_content_mutation();

CREATE TABLE sign_offs (
    sign_off_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    review_scope_id UUID NOT NULL REFERENCES review_scopes(review_scope_id),
    tenant_id UUID NOT NULL,
    workflow_instance_id UUID NOT NULL REFERENCES workflow_instances(workflow_instance_id),
    signed_by_principal_id TEXT NOT NULL,
    content_fingerprint TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'VALID',
    correlation_id TEXT,
    signed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    invalidated_at TIMESTAMPTZ,
    invalidation_reason TEXT,
    CONSTRAINT sign_off_fingerprint_present CHECK (content_fingerprint <> ''),
    CONSTRAINT sign_off_status_valid CHECK (status IN ('VALID','WITHDRAWN','INVALIDATED'))
);
CREATE UNIQUE INDEX sign_off_create_idempotency_unique ON sign_offs (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';
CREATE INDEX sign_offs_scope_valid ON sign_offs (review_scope_id) WHERE status = 'VALID';

-- Only status/invalidated_at/invalidation_reason may ever change on a
-- sign-off — the fingerprint it was bound to, who signed it, and which
-- workflow instance produced it are permanent facts. This is the DB-level
-- half of "sign-off bound to content fingerprint": the binding itself can
-- never be silently rewritten, only invalidated.
CREATE OR REPLACE FUNCTION reject_sign_off_binding_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'sign-offs are never deleted';
    END IF;
    IF OLD.content_fingerprint IS DISTINCT FROM NEW.content_fingerprint
        OR OLD.signed_by_principal_id IS DISTINCT FROM NEW.signed_by_principal_id
        OR OLD.review_scope_id IS DISTINCT FROM NEW.review_scope_id
        OR OLD.workflow_instance_id IS DISTINCT FROM NEW.workflow_instance_id THEN
        RAISE EXCEPTION 'sign-off binding fields are immutable — only status may change';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_reject_sign_off_binding_mutation
    BEFORE UPDATE OR DELETE ON sign_offs
    FOR EACH ROW EXECUTE FUNCTION reject_sign_off_binding_mutation();

CREATE TABLE quality_review_records (
    quality_review_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    engagement_id UUID NOT NULL REFERENCES audit_engagements(engagement_id),
    tenant_id UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'IN_PROGRESS',
    started_by_principal_id TEXT NOT NULL,
    correlation_id TEXT,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_by_principal_id TEXT,
    completed_at TIMESTAMPTZ,
    CONSTRAINT quality_review_status_valid CHECK (status IN ('IN_PROGRESS','COMPLETED'))
);
CREATE UNIQUE INDEX quality_review_create_idempotency_unique ON quality_review_records (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';

ALTER TABLE review_scopes ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_scopes FORCE ROW LEVEL SECURITY;
CREATE POLICY review_scopes_tenant_isolation ON review_scopes FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE review_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY review_assignments_tenant_isolation ON review_assignments FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE review_notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_notes FORCE ROW LEVEL SECURITY;
CREATE POLICY review_notes_tenant_isolation ON review_notes FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE sign_offs ENABLE ROW LEVEL SECURITY;
ALTER TABLE sign_offs FORCE ROW LEVEL SECURITY;
CREATE POLICY sign_offs_tenant_isolation ON sign_offs FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE quality_review_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE quality_review_records FORCE ROW LEVEL SECURITY;
CREATE POLICY quality_review_records_tenant_isolation ON quality_review_records FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
