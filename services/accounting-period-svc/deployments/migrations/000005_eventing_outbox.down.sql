-- Migration: 000005_eventing_outbox.down.sql
-- Removes the eventing outbox. Any undelivered event in it is lost: drain it first.
DROP POLICY IF EXISTS eventing_outbox_tenant_isolation ON eventing_outbox;
ALTER TABLE IF EXISTS eventing_outbox NO FORCE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS eventing_outbox DISABLE ROW LEVEL SECURITY;
DROP TABLE IF EXISTS eventing_outbox;
