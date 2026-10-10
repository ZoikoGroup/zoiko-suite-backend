-- Migration: 000019_add_eventing_outbox.down.sql
--
-- Reverts the transactional event outbox table.

DROP POLICY IF EXISTS tenant_isolation_policy ON eventing_outbox;
DROP INDEX IF EXISTS eventing_outbox_quarantined_idx;
DROP INDEX IF EXISTS eventing_outbox_backlog_idx;
DROP TABLE IF EXISTS eventing_outbox;