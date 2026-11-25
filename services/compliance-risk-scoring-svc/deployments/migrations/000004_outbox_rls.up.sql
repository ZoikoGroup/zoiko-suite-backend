-- Migration 000003: put the transactional outbox under row-level security.
--
-- 000002 left risk_outbox_events without RLS because the relay reads it
-- across tenants. House convention (see accounting-period-svc 000004) is
-- FORCE RLS with one explicit escape hatch: request-path statements are
-- confined to app.tenant_id, and only the relay, which installs
-- app.outbox_relay = 'true' in its own transaction, sees every tenant.
-- NULLIF because a pooled connection keeps a custom GUC as an empty string
-- after a transaction-local set_config resets.

ALTER TABLE risk_outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE risk_outbox_events FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS risk_outbox_events_tenant_isolation ON risk_outbox_events;
CREATE POLICY risk_outbox_events_tenant_isolation ON risk_outbox_events FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
