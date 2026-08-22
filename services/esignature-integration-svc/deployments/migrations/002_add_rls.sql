-- Migration: 002_add_rls.sql
--
-- Row-level security on signature_envelopes, this service's only table.
-- tenant_id is VARCHAR(64) NOT NULL — no nullable/global-scope case here,
-- so the policy is a plain equality check (unlike policy-svc or
-- configuration-feature-flag-svc, where a NULL tenant_id legitimately
-- means "applies to everyone" and must stay readable).
--
-- WITH CHECK matters more than usual on this table. Doc 03 §16.5 makes
-- this service the governed external execution path for contracts, board
-- resolutions and legal artifacts, and UpdateEnvelopeStatus was an
-- unscoped WRITE (`WHERE envelope_id = $4` alone) — any caller holding
-- another tenant's envelope_id could mark that tenant's document signed or
-- completed. USING governs what is visible; WITH CHECK is what refuses a
-- forged transition, so both are stated explicitly rather than relying on
-- USING being reused implicitly.
--
-- Note this migration is only load-bearing BECAUSE the fabricated
-- "default-tenant" fallback in internal/middleware/tenant.go was removed
-- in the same change. With that fallback in place, a header-less request
-- would set app.tenant_id = 'default-tenant' and every such caller would
-- read and write each other's rows — legitimately, inside this policy,
-- while any test passing a real tenant went green.
--
-- tenant_id is VARCHAR here, not UUID, so no ::uuid cast. NULLIF guards
-- against app.tenant_id being set to the empty string, which must never
-- match a real tenant_id — GetTenantID now returns "" rather than a
-- fabricated default precisely so that case matches nothing.
--
-- No platform-scope escape hatch: every route is an HTTP request carrying
-- a verified tenant, and there is no cross-tenant caller.

ALTER TABLE signature_envelopes ENABLE ROW LEVEL SECURITY;
ALTER TABLE signature_envelopes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON signature_envelopes
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
