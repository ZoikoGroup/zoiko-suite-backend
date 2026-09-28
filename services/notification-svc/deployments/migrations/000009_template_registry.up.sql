-- NCD-01: Communication Intent, Template & Content Registry.
--
-- §4.5 seven operations: communication-intents, templates (validate/approve/publish),
-- render-previews, intents/{id}/effective. Plus locale dimension.

CREATE TABLE IF NOT EXISTS communication_intents (
    intent_id          UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    legal_entity_id    VARCHAR(255) NOT NULL,
    name               VARCHAR(255) NOT NULL,           -- e.g., "password_reset", "welcome_email"
    description        TEXT,
    category           VARCHAR(50) NOT NULL,            -- transactional, marketing, regulatory, security
    channels           VARCHAR(100) NOT NULL,           -- comma-separated: EMAIL,IN_APP
    created_by         VARCHAR(255) NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT communication_intents_category_known
        CHECK (category IN ('transactional', 'marketing', 'regulatory', 'security'))
);

CREATE INDEX IF NOT EXISTS idx_communication_intents_lookup
    ON communication_intents (tenant_id, legal_entity_id, name);

ALTER TABLE communication_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE communication_intents FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS communication_intents_tenant_isolation ON communication_intents;
CREATE POLICY communication_intents_tenant_isolation ON communication_intents FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Templates with versioning, approval, and effective-dated publication
CREATE TABLE IF NOT EXISTS templates (
    template_id        UUID PRIMARY KEY,
    intent_id          UUID NOT NULL REFERENCES communication_intents(intent_id) ON DELETE CASCADE,
    tenant_id          VARCHAR(255) NOT NULL,
    legal_entity_id    VARCHAR(255) NOT NULL,
    locale             VARCHAR(10) NOT NULL DEFAULT 'en',  -- ISO 639-1 + optional region
    version            INT NOT NULL DEFAULT 1,
    subject_template   TEXT NOT NULL,
    body_template      TEXT NOT NULL,                       -- HTML template with placeholders
    variables          JSONB NOT NULL DEFAULT '[]',         -- array of required variable names
    status             VARCHAR(20) NOT NULL DEFAULT 'draft', -- draft, pending_approval, approved, published, archived
    approved_by        VARCHAR(255),                        -- principal who approved
    approved_at        TIMESTAMPTZ,
    published_at       TIMESTAMPTZ,
    effective_from     TIMESTAMPTZ,                         -- when this version becomes active
    effective_to       TIMESTAMPTZ,                         -- when this version expires
    created_by         VARCHAR(255) NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT templates_status_known
        CHECK (status IN ('draft', 'pending_approval', 'approved', 'published', 'archived')),
    CONSTRAINT templates_effective_order
        CHECK (effective_from IS NULL OR effective_to IS NULL OR effective_from < effective_to)
);

CREATE INDEX IF NOT EXISTS idx_templates_lookup
    ON templates (tenant_id, legal_entity_id, locale, status);

CREATE INDEX IF NOT EXISTS idx_templates_intent_version
    ON templates (intent_id, version);

ALTER TABLE templates ENABLE ROW LEVEL SECURITY;
ALTER TABLE templates FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS templates_tenant_isolation ON templates;
CREATE POLICY templates_tenant_isolation ON templates FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Template approvals (SoD: creator cannot approve)
CREATE TABLE IF NOT EXISTS template_approvals (
    approval_id        UUID PRIMARY KEY,
    template_id        UUID NOT NULL REFERENCES templates(template_id) ON DELETE CASCADE,
    tenant_id          VARCHAR(255) NOT NULL,
    requested_by       VARCHAR(255) NOT NULL,               -- creator
    approved_by        VARCHAR(255),                        -- approver (different from requested_by)
    status             VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending, approved, rejected
    reason             TEXT,                                -- rejection reason
    requested_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at         TIMESTAMPTZ,

    CONSTRAINT template_approvals_status_known
        CHECK (status IN ('pending', 'approved', 'rejected')),
    CONSTRAINT template_approvals_no_self_approval
        CHECK (approved_by IS NULL OR approved_by != requested_by)
);

CREATE INDEX IF NOT EXISTS idx_template_approvals_template
    ON template_approvals (template_id, status);

ALTER TABLE template_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE template_approvals FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS template_approvals_tenant_isolation ON template_approvals;
CREATE POLICY template_approvals_tenant_isolation ON template_approvals FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Template renders for preview
CREATE TABLE IF NOT EXISTS template_renders (
    render_id          UUID PRIMARY KEY,
    template_id        UUID NOT NULL REFERENCES templates(template_id) ON DELETE CASCADE,
    tenant_id          VARCHAR(255) NOT NULL,
    variables          JSONB NOT NULL,
    rendered_subject   TEXT,
    rendered_body      TEXT,
    error              TEXT,
    created_by         VARCHAR(255) NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_template_renders_template
    ON template_renders (template_id, created_at DESC);

ALTER TABLE template_renders ENABLE ROW LEVEL SECURITY;
ALTER TABLE template_renders FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS template_renders_tenant_isolation ON template_renders;
CREATE POLICY template_renders_tenant_isolation ON template_renders FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));