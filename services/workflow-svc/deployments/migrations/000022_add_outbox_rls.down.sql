-- Migration 000022: Add FORCE RLS to outbox_events - DOWN

ALTER TABLE outbox_events DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;