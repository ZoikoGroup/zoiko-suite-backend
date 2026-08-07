-- 000002_add_rls.up.sql
-- Enable per-tenant Row-Level Security on workflow_history_events.
--
-- tenant_id is NOT NULL on every row (doctrine §3.2), so the policy is a
-- plain equality check.
--
-- This is a defense-in-depth backstop: the application layer must still set
-- app.tenant_id via set_config on every connection (see PgStore.withRLS) for
-- this to have any effect. It does not by itself close the superuser-bypass
-- gap tracked in docs/architecture/known-gaps.md.

ALTER TABLE workflow_history_events ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON workflow_history_events FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));
