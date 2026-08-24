-- Migration: 000002_add_rls.up.sql
--
-- Row-level security on config_entries and feature_flags — the two (and
-- only) tables in this service, both carrying a nullable tenant_id.
--
-- A NULL tenant_id is NOT a missing value here. It is "the global default
-- for this environment", a real and intentional state (see 000001's own
-- comments and the one-effective-row-per-scope partial indexes, which
-- COALESCE NULL to the nil UUID precisely so global rows participate in
-- uniqueness). Those global rows legitimately apply to every tenant, so
-- the policy must keep them readable by everyone — a plain
-- `tenant_id = app.tenant_id` policy would hide every global default from
-- every tenant, which is a silent behaviour change, not added safety:
-- callers would start getting "not found" for configuration that is
-- supposed to apply to them.
--
-- Same doctrine as policy-svc's 000005 and secret-vault-integration-svc's
-- 000003, where nullable scope columns mean the same thing.
--
-- No platform-scope escape hatch is needed here, unlike
-- audit-event-store-svc (Kafka writer with a global hash chain) or
-- authorization-svc (FindGrantedActions on the core /v1/authorize path).
-- Every route in this service is an HTTP request that already resolves a
-- verified tenant via middleware.TenantContext, and every store method
-- already takes that tenant — there is no legitimate cross-tenant caller
-- to exempt.
--
-- current_setting(..., true) — missing_ok = true — returns NULL rather
-- than raising when app.tenant_id is unset, so a connection that forgot
-- to set it sees only global rows instead of erroring. NULLIF guards
-- against it being set to the empty string, which must never match a real
-- tenant_id.

ALTER TABLE config_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE config_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON config_entries
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

ALTER TABLE feature_flags ENABLE ROW LEVEL SECURITY;
ALTER TABLE feature_flags FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON feature_flags
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
