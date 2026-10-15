-- Migration 000009: system role templates and the §9.1 archetypes.
--
-- Authorization Standard §9: a system role template is a "ZoikoSuite-maintained
-- permission bundle for common functions", "versioned; customers cannot mutate
-- canonical template silently". Before this migration there was no template
-- concept at all: every role was a tenant row, so a tenant's "AP Approver" was
-- whatever its administrator happened to type.
--
-- Shape:
--   role_templates          one row per template (platform-wide, no tenant).
--   role_template_versions  the action set of each published version.
--                           APPEND-ONLY: a trigger refuses UPDATE and DELETE,
--                           so a published version can never change under the
--                           roles instantiated from it. A change is a new
--                           version, and a tenant role moves to it only by an
--                           explicit, evented upgrade (POST
--                           /v1/role-definitions/{id}/template-upgrade).
--   role_definitions / permission_bundle_defs gain template_code and
--   template_version: the provenance of a role made from a template, and the
--   marker of the one bundle on it the template manages.
--
-- The §9.1 archetypes are seeded as version 1. Their action sets are derived
-- from each archetype's stated intent in §9.1, written in the §5 taxonomy
-- (000008), and chosen so that no archetype is internally conflicted under the
-- §10.1 baseline authorization-svc seeds in its 000019. The "no ..." clauses of
-- §9.1 (an AP Preparer has no payment release, a Tax Preparer no filing) are
-- honoured by omission. Object-level clauses ("cannot approve own prepared
-- object") are dynamic SoD and are enforced by authorization-svc at decision
-- time, not by a static action list.

CREATE TABLE IF NOT EXISTS role_templates (
    template_code     VARCHAR(100) PRIMARY KEY,
    template_name     VARCHAR(255) NOT NULL,
    default_intent    TEXT         NOT NULL,
    role_scope_type   VARCHAR(30)  NOT NULL CHECK (role_scope_type IN ('LEGAL_ENTITY', 'TENANT')),
    is_archetype      BOOLEAN      NOT NULL DEFAULT FALSE,
    status            VARCHAR(20)  NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DEPRECATED')),
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS role_template_versions (
    template_code      VARCHAR(100) NOT NULL REFERENCES role_templates(template_code),
    template_version   INTEGER      NOT NULL CHECK (template_version > 0),
    permitted_actions  TEXT[]       NOT NULL CHECK (cardinality(permitted_actions) > 0),
    change_note        TEXT         NOT NULL,
    published_by       VARCHAR(255) NOT NULL,
    published_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (template_code, template_version)
);

-- Immutability. RLS alone would not do it: the table owner and a superuser
-- bypass policies, and those are exactly the connections a well-meaning
-- "quick fix" to a template would use. A trigger fires for them too.
CREATE OR REPLACE FUNCTION role_template_versions_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'role_template_versions is append-only: publish a new template_version instead of changing %/%',
        OLD.template_code, OLD.template_version
        USING ERRCODE = 'restrict_violation';
END
$$;

DROP TRIGGER IF EXISTS role_template_versions_no_update ON role_template_versions;
CREATE TRIGGER role_template_versions_no_update
    BEFORE UPDATE OR DELETE ON role_template_versions
    FOR EACH ROW EXECUTE FUNCTION role_template_versions_immutable();

-- Platform-wide and read-only through RLS, like protected_permissions.
ALTER TABLE role_templates ENABLE ROW LEVEL SECURITY;
ALTER TABLE role_templates FORCE ROW LEVEL SECURITY;
ALTER TABLE role_template_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE role_template_versions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS role_templates_read ON role_templates;
CREATE POLICY role_templates_read ON role_templates FOR SELECT USING (true);
DROP POLICY IF EXISTS role_template_versions_read ON role_template_versions;
CREATE POLICY role_template_versions_read ON role_template_versions FOR SELECT USING (true);

ALTER TABLE role_definitions       ADD COLUMN IF NOT EXISTS template_code    VARCHAR(100) REFERENCES role_templates(template_code);
ALTER TABLE role_definitions       ADD COLUMN IF NOT EXISTS template_version INTEGER;
ALTER TABLE permission_bundle_defs ADD COLUMN IF NOT EXISTS template_code    VARCHAR(100) REFERENCES role_templates(template_code);
ALTER TABLE permission_bundle_defs ADD COLUMN IF NOT EXISTS template_version INTEGER;

-- One template-managed bundle per role: it is the bundle an upgrade replaces.
CREATE UNIQUE INDEX IF NOT EXISTS idx_permission_bundle_defs_one_template_bundle
    ON permission_bundle_defs (tenant_id, role_definition_id) WHERE template_code IS NOT NULL;

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('VIEWER_AUDITOR_READ_ONLY',   'Viewer / Auditor Read-Only',  'Read/list/export governed evidence within scope; no mutation.', 'LEGAL_ENTITY', TRUE),
    ('AR_PREPARER',                'AR Preparer',                 'Create/edit/submit customer invoices; no final issue above policy threshold if maker-checker required.', 'LEGAL_ENTITY', TRUE),
    ('AR_APPROVER',                'AR Approver',                 'Approve eligible invoices within authority; cannot approve own prepared object.', 'LEGAL_ENTITY', TRUE),
    ('AP_PREPARER',                'AP Preparer',                 'Create/match supplier invoices; no payment release.', 'LEGAL_ENTITY', TRUE),
    ('AP_APPROVER',                'AP Approver',                 'Approve supplier invoices within authority; independent from preparer where required.', 'LEGAL_ENTITY', TRUE),
    ('TREASURY_PREPARER',          'Treasury Preparer',           'Create payment proposals/instructions; no release.', 'LEGAL_ENTITY', TRUE),
    ('TREASURY_RELEASER',          'Treasury Releaser',           'Release approved payments within bank/entity/amount scope; no self-prepared payment.', 'LEGAL_ENTITY', TRUE),
    ('JOURNAL_PREPARER',           'Journal Preparer',            'Create/submit journals; control-account restrictions apply.', 'LEGAL_ENTITY', TRUE),
    ('JOURNAL_APPROVER',           'Journal Approver',            'Approve/post journals subject to amount/account/period rules and SoD.', 'LEGAL_ENTITY', TRUE),
    ('PERIOD_CONTROLLER',          'Period Controller',           'Execute close checklist; cannot casually reopen.', 'LEGAL_ENTITY', TRUE),
    ('PERIOD_REOPEN_APPROVER',     'Period Reopen Approver',      'Authorize reopen under elevated policy; separate from routine close role.', 'LEGAL_ENTITY', TRUE),
    ('TAX_PREPARER',               'Tax Preparer',                'Prepare tax returns/calculations; no filing authority unless separately granted.', 'LEGAL_ENTITY', TRUE),
    ('TAX_REVIEWER',               'Tax Reviewer',                'Review/approve tax work; qualification/jurisdiction attributes may apply.', 'LEGAL_ENTITY', TRUE),
    ('TAX_FILER',                  'Tax Filer',                   'Submit approved return to authority; credential/authority constraints apply.', 'LEGAL_ENTITY', TRUE),
    ('AUDIT_ENGAGEMENT_MEMBER',    'Audit Engagement Member',     'Access engagement/workpapers assigned by membership relationship.', 'LEGAL_ENTITY', TRUE),
    ('AUDIT_REVIEWER',             'Audit Reviewer',              'Review/sign off workpapers; preparer cannot self-review where policy requires.', 'LEGAL_ENTITY', TRUE),
    ('LEGAL_MATTER_MEMBER',        'Legal Matter Member',         'Access scoped matter/contracts with privilege/classification controls.', 'LEGAL_ENTITY', TRUE),
    ('LEGAL_SIGNATORY',            'Legal Signatory',             'Execute eligible contracts only within authority and entity/jurisdiction limit.', 'LEGAL_ENTITY', TRUE),
    ('IAM_ACCESS_ADMINISTRATOR',   'IAM Access Administrator',    'Manage ordinary access assignments within admin scope; cannot grant own protected privilege.', 'TENANT', TRUE),
    ('SECURITY_PRIVILEGED_OPERATOR','Security Privileged Operator','Eligible for JIT platform privilege; standing data access prohibited.', 'TENANT', TRUE),
    ('SUPPORT_OPERATOR',           'Support Operator',            'Purpose-bound support capabilities; tenant data access requires separately governed support session.', 'TENANT', TRUE)
ON CONFLICT (template_code) DO NOTHING;

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('VIEWER_AUDITOR_READ_ONLY',    1, ARRAY['audit_evidence.read','audit_evidence.list','audit_evidence.export'], 'Initial §9.1 archetype', 'migration:000009'),
    ('AR_PREPARER',                 1, ARRAY['customer_invoice.create','customer_invoice.edit','customer_invoice.submit'], 'Initial §9.1 archetype', 'migration:000009'),
    ('AR_APPROVER',                 1, ARRAY['customer_invoice.approve','customer_invoice.reject','customer_invoice.return'], 'Initial §9.1 archetype', 'migration:000009'),
    ('AP_PREPARER',                 1, ARRAY['supplier_invoice.create','supplier_invoice.edit','supplier_invoice.match','supplier_invoice.submit'], 'Initial §9.1 archetype', 'migration:000009'),
    ('AP_APPROVER',                 1, ARRAY['supplier_invoice.approve','supplier_invoice.reject'], 'Initial §9.1 archetype', 'migration:000009'),
    ('TREASURY_PREPARER',           1, ARRAY['payment.create','payment.prepare','payment.submit'], 'Initial §9.1 archetype', 'migration:000009'),
    ('TREASURY_RELEASER',           1, ARRAY['payment.read','payment.release'], 'Initial §9.1 archetype', 'migration:000009'),
    ('JOURNAL_PREPARER',            1, ARRAY['journal.create','journal.edit','journal.submit'], 'Initial §9.1 archetype', 'migration:000009'),
    ('JOURNAL_APPROVER',            1, ARRAY['journal.approve','journal.reject','journal.post'], 'Initial §9.1 archetype', 'migration:000009'),
    ('PERIOD_CONTROLLER',           1, ARRAY['period.read','period.close','period_close_task.execute'], 'Initial §9.1 archetype', 'migration:000009'),
    ('PERIOD_REOPEN_APPROVER',      1, ARRAY['period.read','period.reopen'], 'Initial §9.1 archetype', 'migration:000009'),
    ('TAX_PREPARER',                1, ARRAY['tax_return.create','tax_return.edit','tax_return.submit'], 'Initial §9.1 archetype', 'migration:000009'),
    ('TAX_REVIEWER',                1, ARRAY['tax_return.approve','tax_return.reject'], 'Initial §9.1 archetype', 'migration:000009'),
    ('TAX_FILER',                   1, ARRAY['tax_return.file'], 'Initial §9.1 archetype', 'migration:000009'),
    ('AUDIT_ENGAGEMENT_MEMBER',     1, ARRAY['workpaper.read','workpaper.create','workpaper.edit','workpaper.submit'], 'Initial §9.1 archetype', 'migration:000009'),
    ('AUDIT_REVIEWER',              1, ARRAY['workpaper.read','workpaper.approve','workpaper.return'], 'Initial §9.1 archetype', 'migration:000009'),
    ('LEGAL_MATTER_MEMBER',         1, ARRAY['legal_matter.read','contract.read','contract.create','contract.edit'], 'Initial §9.1 archetype', 'migration:000009'),
    ('LEGAL_SIGNATORY',             1, ARRAY['contract.read','contract.execute'], 'Initial §9.1 archetype', 'migration:000009'),
    ('IAM_ACCESS_ADMINISTRATOR',    1, ARRAY['iam.assignment.list','iam.assignment.grant','iam.assignment.revoke'], 'Initial §9.1 archetype', 'migration:000009'),
    ('SECURITY_PRIVILEGED_OPERATOR',1, ARRAY['privileged_session.request'], 'Initial §9.1 archetype', 'migration:000009'),
    ('SUPPORT_OPERATOR',            1, ARRAY['support.session.start','support.case.read'], 'Initial §9.1 archetype', 'migration:000009')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- Every template action must be a registered permission (000008). Checked here
-- so a typo in a template fails the migration instead of shipping a role that
-- grants nothing.
DO $$
DECLARE missing TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO missing
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE NOT EXISTS (SELECT 1 FROM permission_definitions p WHERE p.action_name = a AND p.active_flag);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'role template actions not in permission_definitions: %', missing;
    END IF;
END
$$;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_app') THEN
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON role_templates, role_template_versions FROM zoiko_app;
    END IF;
END
$$;
