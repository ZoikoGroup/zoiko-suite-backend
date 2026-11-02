-- Reverses 000004_outbox_rls_build_lease.up.sql.
ALTER TABLE audit_export_requests DROP COLUMN IF EXISTS build_attempts;
ALTER TABLE audit_export_requests DROP COLUMN IF EXISTS build_started_at;

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
ALTER TABLE outbox_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE outbox_events DISABLE ROW LEVEL SECURITY;
