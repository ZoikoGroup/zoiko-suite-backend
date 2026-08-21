-- Migration: 000004_policy_empty_tenant_is_null.up.sql
--
-- Make an EMPTY app.tenant_id filter rather than raise.
--
-- 000003's policies read:
--
--     USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
--
-- `missing_ok = true` was there so a query that forgot the scope matched NO
-- rows instead of erroring, and on a fresh connection that is exactly what
-- happens: current_setting returns NULL and NULL::uuid matches nothing. But a
-- custom GUC does not return to "unset" once it has been touched. After any
-- transaction on that session has run set_config('app.tenant_id', ..., true),
-- the parameter persists for the rest of the SESSION with the value '' -- and
--
--     SELECT ''::uuid  -->  ERROR: invalid input syntax for type uuid: ""
--
-- so the second and every later unscoped query on a POOLED connection got a
-- hard error where the first got an empty result. Intermittent,
-- connection-dependent, and invisible while the service connects as a
-- superuser, because until then the policy is never consulted at all.
--
-- NULLIF(..., '') collapses unset and empty to the same NULL, which is what the
-- `, true` was reaching for.
--
-- applicability_decisions is deliberately absent: it has no tenant_id column and
-- no policy. Its isolation is the tenant-scoped lookup of its parent obligation,
-- which every store method on it performs first.
--
-- Idempotent: DROP POLICY IF EXISTS then CREATE.

DROP POLICY IF EXISTS tenant_isolation_policy ON obligations;
CREATE POLICY tenant_isolation_policy ON obligations
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation_policy ON filing_requirements;
CREATE POLICY tenant_isolation_policy ON filing_requirements
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
