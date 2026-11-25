-- Migration 000004: outbox under RLS, and a lease for in-flight export builds.
--
-- 1. outbox_events was created in 000003 without RLS. House convention
--    (accounting-period-svc 000004) is FORCE RLS with one explicit escape
--    hatch: request-path statements are confined to app.tenant_id, and only
--    the relay, which sets app.outbox_relay = 'true' in its own transaction,
--    sees every tenant. NULLIF because a pooled connection keeps a custom
--    GUC as '' after a transaction-local set_config resets.
--
-- 2. An export build is two transactions with an archive fetch in between
--    (APPROVED -> BUILDING, fetch, BUILDING -> VALIDATING | FAILED). If the
--    process dies after the first commit, the export would sit in BUILDING
--    forever. build_started_at records when the build was claimed so a
--    build whose lease has expired can be re-claimed by a retried
--    POST .../build (ZS-SVC-AC-001: a timed-out worker's outcome is UNKNOWN
--    until the durable package is resolved; no blind duplicate).
--    build_attempts counts claims for evidence.

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );

ALTER TABLE audit_export_requests ADD COLUMN IF NOT EXISTS build_started_at TIMESTAMPTZ;
ALTER TABLE audit_export_requests ADD COLUMN IF NOT EXISTS build_attempts INT NOT NULL DEFAULT 0;
