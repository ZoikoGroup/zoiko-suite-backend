-- BIZ-04 Form Definition & Submission lives beside the audit domain in
-- this same service, per the user's own decision: no new microservice —
-- workflow-svc already hosts several bolted-on BIZ/AUD domains and is
-- the closest structural analog (approval-chain/maker-checker,
-- entity-bound, no-hard-delete) for a submission's validation/approval
-- lifecycle.
--
-- FormDefinition owns identity/purpose/target-domain/schema/owner.
-- FormSubmission owns the actual immutable submitted values and its own
-- lifecycle (Draft -> Submitted -> Validating -> Accepted/Rejected ->
-- Superseded) — same split as document-vault-svc's Document/Version and
-- notification-svc's TemplateDefinition/TemplateVersion, for the same
-- reason: identity is cheap to keep flexible, content must be governed.

CREATE TABLE form_definitions (
    form_id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    legal_entity_id            UUID NOT NULL,
    name                       TEXT NOT NULL,
    business_purpose           TEXT NOT NULL,
    -- target_domain is an opaque caller-supplied string (e.g.
    -- "EXPENSE_CLAIM", "PROCUREMENT_REQUEST") — no FK, no generic
    -- dispatcher exists in this codebase for "which service owns this
    -- target domain", same opaque-reference posture as
    -- document-vault-svc's document_links.linked_object_type.
    target_domain               TEXT NOT NULL,
    -- Required field names, e.g. '["employee_id", "amount"]' — same
    -- contract shape as notification-svc's template_versions.variable_schema.
    schema                      JSONB NOT NULL DEFAULT '[]'::jsonb,
    owner_principal_id          TEXT NOT NULL,
    status                       TEXT NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'PUBLISHED', 'RETIRED')),
    version                      INTEGER NOT NULL DEFAULT 1,
    published_by_principal_id    TEXT,
    published_at                 TIMESTAMPTZ,
    retired_by_principal_id      TEXT,
    retired_at                   TIMESTAMPTZ,
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    correlation_id               TEXT,
    CONSTRAINT form_definition_name_present CHECK (name <> ''),
    CONSTRAINT form_definition_purpose_present CHECK (business_purpose <> ''),
    CONSTRAINT form_definition_target_domain_present CHECK (target_domain <> ''),
    CONSTRAINT form_definition_owner_present CHECK (owner_principal_id <> ''),
    -- Maker-checker as a DB CHECK, not just app-layer enforcement — same
    -- pattern as document-vault-svc's record_classifications and
    -- notification-svc's template_versions. Applied to EVERY form, not
    -- only ones flagged sensitive: this service has no real
    -- classification of which target-domain mappings count as that, and
    -- does not invent one (same resolution the user already made for
    -- BIZ-03's template approval scope).
    CONSTRAINT form_definition_no_self_publish
        CHECK (published_by_principal_id IS NULL OR published_by_principal_id <> owner_principal_id)
);
CREATE UNIQUE INDEX form_definition_create_idempotency_unique ON form_definitions (tenant_id, correlation_id)
    WHERE correlation_id IS NOT NULL AND correlation_id <> '';
CREATE INDEX form_definitions_tenant_entity ON form_definitions (tenant_id, legal_entity_id);

CREATE TABLE form_submissions (
    submission_id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    form_id                       UUID NOT NULL REFERENCES form_definitions(form_id),
    tenant_id                     UUID NOT NULL,
    legal_entity_id               UUID NOT NULL,
    -- Snapshot of form_definitions.version at SubmitForm time — the form
    -- may not be retired/changed after a submission is validated against
    -- a version that no longer matches "the latest".
    form_version                  INTEGER NOT NULL,
    status                        TEXT NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'SUBMITTED', 'VALIDATING', 'ACCEPTED', 'REJECTED', 'SUPERSEDED')),
    submitted_values               JSONB NOT NULL DEFAULT '{}'::jsonb,
    submitter_principal_id         TEXT NOT NULL,
    consent_attestation            TEXT,
    validation_result              JSONB,
    rejection_reason                TEXT,
    superseded_by_submission_id     UUID REFERENCES form_submissions(submission_id),
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    submitted_at                     TIMESTAMPTZ,
    validated_at                     TIMESTAMPTZ,
    correlation_id                   TEXT,
    CONSTRAINT form_submission_submitter_present CHECK (submitter_principal_id <> ''),
    CONSTRAINT form_submission_rejection_has_reason
        CHECK (status <> 'REJECTED' OR (rejection_reason IS NOT NULL AND rejection_reason <> ''))
);
CREATE UNIQUE INDEX form_submission_create_idempotency_unique ON form_submissions (tenant_id, correlation_id)
    WHERE correlation_id IS NOT NULL AND correlation_id <> '';
CREATE INDEX form_submissions_form ON form_submissions (form_id, created_at DESC);
CREATE INDEX form_submissions_pending ON form_submissions (tenant_id, status) WHERE status IN ('SUBMITTED', 'VALIDATING');

-- BIZ-04's own record of a downstream routing attempt — deliberately
-- separate from form_submissions.status, per the doc's own failure
-- semantics: "target-domain rejection leaves submission accepted as
-- evidence but not as successful business action." A submission stays
-- ACCEPTED forever once accepted; whether it was ever successfully
-- routed, and to what, is this table's job to say, not a status
-- overwrite on the submission itself.
CREATE TABLE form_submission_routes (
    route_id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    submission_id              UUID NOT NULL REFERENCES form_submissions(submission_id),
    tenant_id                  UUID NOT NULL,
    target_domain               TEXT NOT NULL,
    command_reference            TEXT,
    result_reference              TEXT,
    outcome                        TEXT NOT NULL CHECK (outcome IN ('SUCCEEDED', 'FAILED')),
    failure_reason                  TEXT,
    routed_by_principal_id           TEXT NOT NULL,
    routed_at                        TIMESTAMPTZ NOT NULL DEFAULT now(),
    correlation_id                    TEXT,
    CONSTRAINT form_submission_route_target_domain_present CHECK (target_domain <> ''),
    CONSTRAINT form_submission_route_actor_present CHECK (routed_by_principal_id <> ''),
    CONSTRAINT form_submission_route_failure_has_reason
        CHECK (outcome <> 'FAILED' OR (failure_reason IS NOT NULL AND failure_reason <> ''))
);
CREATE INDEX form_submission_routes_submission ON form_submission_routes (submission_id, routed_at);

ALTER TABLE form_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE form_definitions FORCE ROW LEVEL SECURITY;
CREATE POLICY form_definitions_tenant_isolation ON form_definitions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE form_submissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE form_submissions FORCE ROW LEVEL SECURITY;
CREATE POLICY form_submissions_tenant_isolation ON form_submissions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE form_submission_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE form_submission_routes FORCE ROW LEVEL SECURITY;
CREATE POLICY form_submission_routes_tenant_isolation ON form_submission_routes FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Content is immutable from creation; once RETIRED, the row is frozen
-- outright.
CREATE OR REPLACE FUNCTION reject_form_definition_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'form definitions are never deleted';
    END IF;
    IF NEW.name IS DISTINCT FROM OLD.name OR NEW.business_purpose IS DISTINCT FROM OLD.business_purpose
        OR NEW.target_domain IS DISTINCT FROM OLD.target_domain OR NEW.schema IS DISTINCT FROM OLD.schema
        OR NEW.owner_principal_id IS DISTINCT FROM OLD.owner_principal_id
    THEN
        RAISE EXCEPTION 'form definition content is immutable once created (form %)', OLD.form_id;
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'form % is retired and cannot be changed further', OLD.form_id;
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'form % is already published; publish date cannot change', OLD.form_id;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_reject_form_definition_mutation
    BEFORE UPDATE OR DELETE ON form_definitions
    FOR EACH ROW EXECUTE FUNCTION reject_form_definition_mutation();

-- submitted_values/consent_attestation may change freely while DRAFT
-- (SaveDraft) but become immutable the moment the submission leaves
-- DRAFT — the doc's own "immutable submissions" purpose line. Once
-- SUPERSEDED, the row is frozen outright.
CREATE OR REPLACE FUNCTION reject_form_submission_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'form submissions are never deleted';
    END IF;
    IF NEW.form_id IS DISTINCT FROM OLD.form_id OR NEW.form_version IS DISTINCT FROM OLD.form_version
        OR NEW.submitter_principal_id IS DISTINCT FROM OLD.submitter_principal_id
    THEN
        RAISE EXCEPTION 'form submission identity fields are immutable (submission %)', OLD.submission_id;
    END IF;
    IF OLD.status = 'SUPERSEDED' THEN
        RAISE EXCEPTION 'submission % is superseded and cannot be changed further', OLD.submission_id;
    END IF;
    IF OLD.status <> 'DRAFT' AND (NEW.submitted_values IS DISTINCT FROM OLD.submitted_values
        OR NEW.consent_attestation IS DISTINCT FROM OLD.consent_attestation)
    THEN
        RAISE EXCEPTION 'submitted values are immutable once submitted (submission %)', OLD.submission_id;
    END IF;
    IF OLD.superseded_by_submission_id IS NOT NULL AND NEW.superseded_by_submission_id IS DISTINCT FROM OLD.superseded_by_submission_id THEN
        RAISE EXCEPTION 'submission % has already been superseded; this cannot change', OLD.submission_id;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_reject_form_submission_mutation
    BEFORE UPDATE OR DELETE ON form_submissions
    FOR EACH ROW EXECUTE FUNCTION reject_form_submission_mutation();

-- Routing evidence is append-only, same as every other evidence trail in
-- this build.
CREATE OR REPLACE FUNCTION reject_form_submission_route_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'form submission routes are append-only';
END;
$$;
CREATE TRIGGER trg_reject_form_submission_route_mutation
    BEFORE UPDATE OR DELETE ON form_submission_routes
    FOR EACH ROW EXECUTE FUNCTION reject_form_submission_route_mutation();
