-- Migration: 000002_outbox_events_relay_policy.down.sql
--
-- Restores 000001's tenant-only policy (which locks the relay out under any
-- NOBYPASSRLS role — see the up migration).

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
