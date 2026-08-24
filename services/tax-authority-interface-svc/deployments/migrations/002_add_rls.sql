-- Migration: 002_add_rls.sql
--
-- Row-level security on tax_interfaces and tax_filing_submissions. Both carry
-- tenant_id VARCHAR(64) NOT NULL — no nullable/global-scope case here, so
-- the policy is a plain equality check (unlike policy-svc or
-- configuration-feature-flag-svc, where a NULL tenant_id legitimately
-- means "applies to everyone" and must stay readable).
--
-- Note this migration is only load-bearing BECAUSE the fabricated
-- "default-tenant" fallback in internal/middleware/tenant.go was removed
-- in the same change. With that fallback in place, a header-less request
-- would set app.tenant_id = 'default-tenant' and every such caller would
-- read and write each other's rows — legitimately, inside this policy,
-- while any test passing a real tenant went green. The policy would have
-- looked like tenant isolation without being one.
--
-- tenant_id is VARCHAR here, not UUID, so no ::uuid cast. NULLIF guards
-- against app.tenant_id being set to the empty string, which must never
-- match a real tenant_id — GetTenantID now returns "" rather than a
-- fabricated default precisely so that case matches nothing.
--
-- No platform-scope escape hatch: every route in this service is an HTTP
-- request carrying a verified tenant, and there is no cross-tenant caller
-- (no Kafka consumer, no global chain, no platform-authorized admin
-- action) — unlike audit-event-store-svc or authorization-svc.

ALTER TABLE tax_interfaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_interfaces FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON tax_interfaces
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE tax_filing_submissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_filing_submissions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON tax_filing_submissions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
