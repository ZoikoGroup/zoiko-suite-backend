-- 000032_tenant_row_security.up.sql
--
-- R4 (implementation plan section 2): tenant data is isolated by the database, not
-- only by a WHERE clause. Jurisdiction packs, rules, calendars and terms are
-- platform reference data (ZS-JUR-001 s3: "packs are not tenant data") and keep no
-- tenant. Three tables hold a tenant's own facts:
--
--   entity_jurisdiction_applicability  a tenant's entities and their jurisdictions
--   regulatory_submissions (+ events)  a tenant's filings and e-invoices
--   rule_decision_evidence             the facts a tenant's request was decided on
--
-- Each gets ENABLE + FORCE ROW LEVEL SECURITY keyed on app.tenant_id, which the
-- service sets in every transaction (store.Unit for writes, the scoped reads for
-- GETs, the consumer per event). FORCE makes the policies bind the table owner too;
-- only a superuser (migrations, break-glass) bypasses them.
--
-- Submissions and decision evidence had no tenant column. It is added with a
-- default read from app.tenant_id, so every insert records the tenant it ran for
-- without the insert statements naming it. Rows written before this migration have
-- no tenant and cannot be attributed to one:
--   * legacy submissions are no longer visible to any tenant (a NOT VALID check
--     makes every new or changed submission carry a tenant);
--   * legacy decisions, like decisions made for a caller with no tenant (a service
--     resolving platform rules), stay visible to callers without a tenant.
--
-- Platform operators read decisions across tenants for three governed purposes:
-- remediation of a withdrawn or rolled-back pack version (s31), the rollback's
-- count of affected decisions (s23) and the s29 operations metrics. Those store
-- paths set app.cross_tenant = 'on' for their own transaction, after the handler
-- has authorized the operator; nothing else does.

-- ── entity_jurisdiction_applicability ───────────────────────────────────────
ALTER TABLE entity_jurisdiction_applicability ENABLE ROW LEVEL SECURITY;
ALTER TABLE entity_jurisdiction_applicability FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON entity_jurisdiction_applicability;
CREATE POLICY tenant_isolation ON entity_jurisdiction_applicability
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- ── regulatory_submissions and their events ─────────────────────────────────
ALTER TABLE regulatory_submissions
    ADD COLUMN IF NOT EXISTS tenant_id TEXT DEFAULT NULLIF(current_setting('app.tenant_id', true), '');
ALTER TABLE regulatory_submissions
    ADD CONSTRAINT ck_submission_has_tenant CHECK (tenant_id IS NOT NULL) NOT VALID;
CREATE INDEX IF NOT EXISTS idx_submission_tenant ON regulatory_submissions (tenant_id, created_at DESC);
ALTER TABLE regulatory_submissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE regulatory_submissions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON regulatory_submissions;
CREATE POLICY tenant_isolation ON regulatory_submissions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE regulatory_submission_events
    ADD COLUMN IF NOT EXISTS tenant_id TEXT DEFAULT NULLIF(current_setting('app.tenant_id', true), '');
ALTER TABLE regulatory_submission_events
    ADD CONSTRAINT ck_submission_event_has_tenant CHECK (tenant_id IS NOT NULL) NOT VALID;
ALTER TABLE regulatory_submission_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE regulatory_submission_events FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON regulatory_submission_events;
CREATE POLICY tenant_isolation ON regulatory_submission_events
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- ── rule_decision_evidence ──────────────────────────────────────────────────
-- Adding a column fires no row trigger, so the append-only guards are untouched.
ALTER TABLE rule_decision_evidence
    ADD COLUMN IF NOT EXISTS tenant_id TEXT DEFAULT NULLIF(current_setting('app.tenant_id', true), '');
CREATE INDEX IF NOT EXISTS idx_rule_decision_tenant ON rule_decision_evidence (tenant_id, created_at DESC);
ALTER TABLE rule_decision_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE rule_decision_evidence FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON rule_decision_evidence;
CREATE POLICY tenant_isolation ON rule_decision_evidence
    USING (tenant_id IS NOT DISTINCT FROM NULLIF(current_setting('app.tenant_id', true), '')
           OR current_setting('app.cross_tenant', true) = 'on')
    WITH CHECK (tenant_id IS NOT DISTINCT FROM NULLIF(current_setting('app.tenant_id', true), ''));
