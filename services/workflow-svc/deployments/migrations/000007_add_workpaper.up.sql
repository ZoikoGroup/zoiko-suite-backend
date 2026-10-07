-- AUD-07 Workpaper lives beside AUD-01/02 in this same service — a
-- workpaper's own lock digest is what AUD-09 sign-offs bind to, and
-- AUD-01's own MarkFieldworkComplete gate must see "every required
-- workpaper is LOCKED" synchronously.

CREATE TABLE workpapers (
    workpaper_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    engagement_id UUID NOT NULL REFERENCES audit_engagements(engagement_id),
    tenant_id UUID NOT NULL,
    reference TEXT NOT NULL,
    purpose TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'DRAFT',
    required BOOLEAN NOT NULL DEFAULT true,
    has_contradiction_flag BOOLEAN NOT NULL DEFAULT false,
    prepared_by_principal_id TEXT,
    prepared_at TIMESTAMPTZ,
    reviewed_at TIMESTAMPTZ,
    lock_digest TEXT,
    locked_by_principal_id TEXT,
    locked_at TIMESTAMPTZ,
    created_by_principal_id TEXT NOT NULL,
    correlation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT workpaper_reference_present CHECK (reference <> ''),
    CONSTRAINT workpaper_status_valid CHECK (status IN ('DRAFT','IN_PROGRESS','PREPARED','REVIEWED','LOCKED')),
    CONSTRAINT workpaper_creator_present CHECK (created_by_principal_id <> ''),
    CONSTRAINT workpaper_lock_complete CHECK (
        (status <> 'LOCKED') OR (lock_digest IS NOT NULL AND locked_by_principal_id IS NOT NULL AND locked_at IS NOT NULL)
    )
);
CREATE UNIQUE INDEX workpaper_reference_unique ON workpapers (engagement_id, reference);
CREATE UNIQUE INDEX workpaper_create_idempotency_unique ON workpapers (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL AND correlation_id <> '';
CREATE INDEX workpapers_engagement_required_unlocked ON workpapers (engagement_id) WHERE required = true AND status <> 'LOCKED';

CREATE TABLE workpaper_procedures (
    procedure_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workpaper_id UUID NOT NULL REFERENCES workpapers(workpaper_id),
    tenant_id UUID NOT NULL,
    description TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT workpaper_procedure_description_present CHECK (description <> '')
);

CREATE TABLE workpaper_results (
    result_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workpaper_id UUID NOT NULL REFERENCES workpapers(workpaper_id),
    tenant_id UUID NOT NULL,
    description TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT workpaper_result_description_present CHECK (description <> '')
);

CREATE TABLE workpaper_conclusions (
    conclusion_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workpaper_id UUID NOT NULL REFERENCES workpapers(workpaper_id),
    tenant_id UUID NOT NULL,
    description TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT workpaper_conclusion_description_present CHECK (description <> '')
);

CREATE TABLE workpaper_cross_references (
    cross_reference_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    from_workpaper_id UUID NOT NULL REFERENCES workpapers(workpaper_id),
    to_workpaper_id UUID NOT NULL REFERENCES workpapers(workpaper_id),
    tenant_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT workpaper_cross_reference_not_self CHECK (from_workpaper_id <> to_workpaper_id)
);

CREATE TABLE workpaper_evidence_links (
    link_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workpaper_id UUID NOT NULL REFERENCES workpapers(workpaper_id),
    tenant_id UUID NOT NULL,
    evidence_id TEXT NOT NULL,
    contradiction_flag BOOLEAN NOT NULL DEFAULT false,
    linked_by_principal_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT workpaper_evidence_id_present CHECK (evidence_id <> '')
);

CREATE TABLE workpaper_addenda (
    addendum_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workpaper_id UUID NOT NULL REFERENCES workpapers(workpaper_id),
    tenant_id UUID NOT NULL,
    added_by_principal_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    effect TEXT NOT NULL,
    content TEXT NOT NULL DEFAULT '',
    added_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT workpaper_addendum_reason_present CHECK (reason <> ''),
    CONSTRAINT workpaper_addendum_effect_present CHECK (effect <> ''),
    CONSTRAINT workpaper_addendum_actor_present CHECK (added_by_principal_id <> '')
);

ALTER TABLE workpapers ENABLE ROW LEVEL SECURITY;
ALTER TABLE workpapers FORCE ROW LEVEL SECURITY;
CREATE POLICY workpapers_tenant_isolation ON workpapers FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE workpaper_procedures ENABLE ROW LEVEL SECURITY;
ALTER TABLE workpaper_procedures FORCE ROW LEVEL SECURITY;
CREATE POLICY workpaper_procedures_tenant_isolation ON workpaper_procedures FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE workpaper_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE workpaper_results FORCE ROW LEVEL SECURITY;
CREATE POLICY workpaper_results_tenant_isolation ON workpaper_results FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE workpaper_conclusions ENABLE ROW LEVEL SECURITY;
ALTER TABLE workpaper_conclusions FORCE ROW LEVEL SECURITY;
CREATE POLICY workpaper_conclusions_tenant_isolation ON workpaper_conclusions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE workpaper_cross_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE workpaper_cross_references FORCE ROW LEVEL SECURITY;
CREATE POLICY workpaper_cross_references_tenant_isolation ON workpaper_cross_references FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE workpaper_evidence_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE workpaper_evidence_links FORCE ROW LEVEL SECURITY;
CREATE POLICY workpaper_evidence_links_tenant_isolation ON workpaper_evidence_links FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE workpaper_addenda ENABLE ROW LEVEL SECURITY;
ALTER TABLE workpaper_addenda FORCE ROW LEVEL SECURITY;
CREATE POLICY workpaper_addenda_tenant_isolation ON workpaper_addenda FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- "Prior versions retained" / lock immutability: once a workpaper is
-- LOCKED, no UPDATE or DELETE reaches it or its child evidentiary rows.
-- All subsequent additions route through workpaper_addenda, which is
-- itself unconditionally append-only.
CREATE OR REPLACE FUNCTION reject_locked_workpaper_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status = 'LOCKED' THEN
            RAISE EXCEPTION 'a locked workpaper cannot be deleted';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.status = 'LOCKED' THEN
        RAISE EXCEPTION 'a locked workpaper is immutable — use AddPostLockAddendum instead';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_reject_locked_workpaper_mutation
    BEFORE UPDATE OR DELETE ON workpapers
    FOR EACH ROW EXECUTE FUNCTION reject_locked_workpaper_mutation();

CREATE OR REPLACE FUNCTION reject_locked_workpaper_child_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    wp_status TEXT;
BEGIN
    SELECT status INTO wp_status FROM workpapers WHERE workpaper_id = COALESCE(OLD.workpaper_id, NEW.workpaper_id);
    IF wp_status = 'LOCKED' THEN
        RAISE EXCEPTION 'cannot modify a child row of a locked workpaper';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_reject_locked_procedure_mutation
    BEFORE UPDATE OR DELETE ON workpaper_procedures
    FOR EACH ROW EXECUTE FUNCTION reject_locked_workpaper_child_mutation();
CREATE TRIGGER trg_reject_locked_result_mutation
    BEFORE UPDATE OR DELETE ON workpaper_results
    FOR EACH ROW EXECUTE FUNCTION reject_locked_workpaper_child_mutation();
CREATE TRIGGER trg_reject_locked_conclusion_mutation
    BEFORE UPDATE OR DELETE ON workpaper_conclusions
    FOR EACH ROW EXECUTE FUNCTION reject_locked_workpaper_child_mutation();
CREATE TRIGGER trg_reject_locked_evidence_link_mutation
    BEFORE UPDATE OR DELETE ON workpaper_evidence_links
    FOR EACH ROW EXECUTE FUNCTION reject_locked_workpaper_child_mutation();

CREATE OR REPLACE FUNCTION reject_workpaper_addendum_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'workpaper addenda are append-only';
END;
$$;
CREATE TRIGGER trg_reject_workpaper_addendum_mutation
    BEFORE UPDATE OR DELETE ON workpaper_addenda
    FOR EACH ROW EXECUTE FUNCTION reject_workpaper_addendum_mutation();
