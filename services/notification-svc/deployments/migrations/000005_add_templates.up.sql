-- BIZ-03 Template — TemplateDefinition + TemplateVersion.
--
-- TemplateDefinition owns identity/purpose/owner. TemplateVersion owns the
-- actual governed content and carries its own lifecycle
-- (Draft -> Review -> Approved -> Published -> Retired/Superseded) — a
-- template can have several locales, each maturing independently, so the
-- state machine belongs on the version, not the definition. This mirrors
-- document-vault-svc's Document/DocumentVersion split for the same reason.

CREATE TABLE template_definitions (
    template_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              VARCHAR(255) NOT NULL,
    legal_entity_id        VARCHAR(255) NOT NULL,
    name                   VARCHAR(255) NOT NULL,
    business_purpose       TEXT NOT NULL,
    owner_principal_id     VARCHAR(255) NOT NULL,
    status                 VARCHAR(20) NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'RETIRED')),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at             TIMESTAMPTZ,
    retired_by_principal_id VARCHAR(255)
);

CREATE TABLE template_versions (
    version_id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id             UUID NOT NULL REFERENCES template_definitions(template_id),
    tenant_id                VARCHAR(255) NOT NULL,
    legal_entity_id          VARCHAR(255) NOT NULL,
    version_number           INTEGER NOT NULL,
    locale                   VARCHAR(20) NOT NULL,
    content                  TEXT NOT NULL,
    content_hash             VARCHAR(64) NOT NULL,
    -- Required variable names, e.g. '["organization_name", "login_url"]' —
    -- same contract shape as internal/templates' existing static catalogue,
    -- now governed data instead of compiled-in Go.
    variable_schema          JSONB NOT NULL DEFAULT '[]'::jsonb,
    branding_metadata        JSONB,
    accessibility_metadata   JSONB,
    status                   VARCHAR(20) NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'REVIEW', 'APPROVED', 'PUBLISHED', 'RETIRED', 'SUPERSEDED')),
    created_by_principal_id  VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    validated_at             TIMESTAMPTZ,
    approved_by_principal_id VARCHAR(255),
    approved_at              TIMESTAMPTZ,
    published_at             TIMESTAMPTZ,
    retired_at               TIMESTAMPTZ,
    superseded_by_version_id UUID REFERENCES template_versions(version_id),
    -- A maker-checker CHECK, not just app-layer enforcement — see
    -- document-vault-svc's record_classifications for the identical
    -- pattern. Wave 1 decision: EVERY template requires a different
    -- approver than its author, not only ones flagged "externally
    -- binding" — this service has no real classification of which
    -- templates count as that, so it does not invent one.
    CHECK (approved_by_principal_id IS NULL OR approved_by_principal_id <> created_by_principal_id),
    CHECK ((approved_at IS NULL AND approved_by_principal_id IS NULL) OR (approved_at IS NOT NULL AND approved_by_principal_id IS NOT NULL)),
    UNIQUE (template_id, locale, version_number)
);

CREATE INDEX idx_template_versions_template ON template_versions (template_id, locale, version_number DESC);
CREATE INDEX idx_template_versions_published ON template_versions (template_id, locale, status) WHERE status = 'PUBLISHED';

ALTER TABLE template_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE template_definitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON template_definitions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE template_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE template_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON template_versions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Content is immutable from the moment a version is created — not just
-- once approved. Once a version reaches a terminal state (RETIRED or
-- SUPERSEDED) the row is frozen outright: no field may change again,
-- which also protects approved_at/published_at/superseded_by_version_id
-- from any further mutation without needing a separate check for each.
CREATE OR REPLACE FUNCTION reject_template_version_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'template_versions rows are never deleted';
    END IF;
    IF NEW.template_id IS DISTINCT FROM OLD.template_id
        OR NEW.version_number IS DISTINCT FROM OLD.version_number
        OR NEW.locale IS DISTINCT FROM OLD.locale
        OR NEW.content IS DISTINCT FROM OLD.content
        OR NEW.content_hash IS DISTINCT FROM OLD.content_hash
        OR NEW.variable_schema IS DISTINCT FROM OLD.variable_schema
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
    THEN
        RAISE EXCEPTION 'template_versions content is immutable once written (version %)', OLD.version_id;
    END IF;
    IF OLD.status IN ('RETIRED', 'SUPERSEDED') THEN
        RAISE EXCEPTION 'version % is in a terminal state (%) and cannot be changed further', OLD.version_id, OLD.status;
    END IF;
    IF OLD.approved_at IS NOT NULL AND (NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approved_by_principal_id IS DISTINCT FROM OLD.approved_by_principal_id) THEN
        RAISE EXCEPTION 'version % is already approved; approval cannot change', OLD.version_id;
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'version % is already published; publish date cannot change', OLD.version_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_template_version_mutation
    BEFORE UPDATE OR DELETE ON template_versions
    FOR EACH ROW EXECUTE FUNCTION reject_template_version_mutation();
