-- Rollback: drop the audit_events table and its indexes.
-- Indexes are dropped automatically by DROP TABLE.
DROP TABLE IF EXISTS audit_events;
