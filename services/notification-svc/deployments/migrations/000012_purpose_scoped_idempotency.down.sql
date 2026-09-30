-- Reverse of 000012.
--
-- Restoring the full (tenant_id, correlation_id) unique index FAILS if two
-- purpose-scoped notifications share a correlation id — which is exactly the
-- case 000012 exists to allow. Find them first:
--
--   SELECT tenant_id, correlation_id, count(*) FROM notifications
--   GROUP BY 1, 2 HAVING count(*) > 1;
--
-- and decide what to do with them before running this. Roll the binary back
-- with it: the store's ON CONFLICT names the partial indexes below.
DROP INDEX IF EXISTS idx_notifications_tenant_idempotency;
DROP INDEX IF EXISTS idx_notifications_tenant_correlation_unkeyed;
CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_tenant_correlation
    ON notifications (tenant_id, correlation_id);
ALTER TABLE notifications DROP COLUMN IF EXISTS idempotency_key;
ALTER TABLE notifications DROP COLUMN IF EXISTS purpose_context;
