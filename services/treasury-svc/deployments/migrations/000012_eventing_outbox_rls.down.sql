-- Migration: 000012_eventing_outbox_rls.down.sql
-- Restores 000011's state: no RLS on eventing_outbox.

DROP POLICY IF EXISTS eventing_outbox_tenant_isolation ON eventing_outbox;
ALTER TABLE eventing_outbox NO FORCE ROW LEVEL SECURITY;
ALTER TABLE eventing_outbox DISABLE ROW LEVEL SECURITY;
