-- Migration: 000005_policy_empty_tenant_is_null.up.sql
--
-- Make an EMPTY app.tenant_id filter rather than raise.
--
-- The policy this replaces read:
--
--     USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
--
-- `missing_ok = true` was there so a query that forgot to set the scope matched
-- NO rows instead of erroring, and on a fresh connection that is what happens:
-- current_setting returns NULL and NULL::UUID matches nothing. But a custom GUC
-- does not go back to "unset" once it has been touched. After any transaction on
-- that session has run set_config('app.tenant_id', ..., true), the parameter
-- persists for the rest of the SESSION with the value '' -- and
--
--     SELECT ''::UUID  -->  ERROR: invalid input syntax for type uuid: ""
--
-- so the second and every later unscoped query on a POOLED connection got a hard
-- error where the first got an empty result. Intermittent, connection-dependent,
-- and invisible until the service stops connecting as a superuser, because until
-- then the policy is never consulted at all.
--
-- NULLIF(..., '') collapses unset and empty to the same NULL, which is what the
-- original `, true` was reaching for.
--
-- Idempotent: DROP POLICY IF EXISTS then CREATE.

DROP POLICY IF EXISTS tenant_isolation_policy ON journal_headers;
CREATE POLICY tenant_isolation_policy ON journal_headers
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY IF EXISTS tenant_isolation_policy ON journal_lines;
CREATE POLICY tenant_isolation_policy ON journal_lines
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
