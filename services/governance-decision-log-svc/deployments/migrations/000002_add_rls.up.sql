-- 000002_add_rls.up.sql
-- Enable per-tenant Row-Level Security on governance_decisions.
--
-- governance_decisions.tenant_id is NOT NULL on every row (no global/shared
-- decisions exist), so the policy is a plain equality check — no NULL
-- carve-out needed, unlike policy-svc's nullable-tenant_id policy_versions.
--
-- This is a defense-in-depth backstop: the application layer must still set
-- app.tenant_id via set_config on every connection (see PgStore.withRLS) for
-- this to have any effect. It does not by itself close the superuser-bypass
-- gap tracked in docs/architecture/known-gaps.md (RLS is bypassed by a
-- superuser or BYPASSRLS role — the app's DB role must be neither).

ALTER TABLE governance_decisions ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON governance_decisions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));
