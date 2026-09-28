-- Down migration for 000009_template_registry.up.sql

DROP POLICY IF EXISTS template_renders_tenant_isolation ON template_renders;
DROP TABLE IF EXISTS template_renders;

DROP POLICY IF EXISTS template_approvals_tenant_isolation ON template_approvals;
DROP TABLE IF EXISTS template_approvals;

DROP POLICY IF EXISTS templates_tenant_isolation ON templates;
DROP TABLE IF EXISTS templates;

DROP POLICY IF EXISTS communication_intents_tenant_isolation ON communication_intents;
DROP TABLE IF EXISTS communication_intents;