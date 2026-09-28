-- Down migration for 000006_delivery_attempts_and_status.up.sql

DROP INDEX IF EXISTS idx_notifications_tenant_idempotency;

-- Recreate the old idempotency index (tenant_id, correlation_id)
-- Note: this will only work if the data doesn't have conflicting keys
CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_tenant_correlation
    ON notifications (tenant_id, correlation_id);

-- Restore the old status constraint
DO $$
BEGIN
    ALTER TABLE notifications
        DROP CONSTRAINT IF EXISTS notifications_status_known;
    ALTER TABLE notifications
        DROP CONSTRAINT IF EXISTS notifications_concluded_has_timestamp;

    ALTER TABLE notifications
        ADD CONSTRAINT notifications_status_known
        CHECK (status IN ('PENDING', 'SENT', 'FAILED')) NOT VALID;

    ALTER TABLE notifications
        ADD CONSTRAINT notifications_concluded_has_timestamp
        CHECK (status = 'PENDING' OR sent_at IS NOT NULL) NOT VALID;
END $$;

DROP TABLE IF EXISTS delivery_attempts;