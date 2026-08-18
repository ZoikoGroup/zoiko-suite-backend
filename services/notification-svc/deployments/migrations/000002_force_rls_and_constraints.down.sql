DROP INDEX IF EXISTS idx_notifications_tenant_created;

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_failed_has_reason;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_concluded_has_timestamp;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_status_known;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_channel_known;

DROP POLICY IF EXISTS tenant_isolation_policy ON notifications;
CREATE POLICY tenant_isolation_policy ON notifications FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE notifications NO FORCE ROW LEVEL SECURITY;
