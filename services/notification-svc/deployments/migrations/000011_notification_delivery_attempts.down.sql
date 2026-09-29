-- Reverse of 000011. Discards the per-attempt evidence chain for every
-- direct-send notification; the notifications rows keep only their counter and
-- last-attempt fields. Roll the binary back with it: the transitions insert
-- into this table in the same transaction, so every send would fail.
DROP POLICY IF EXISTS nda_platform_scope_read ON notification_delivery_attempts;
DROP POLICY IF EXISTS nda_tenant_isolation ON notification_delivery_attempts;
DROP INDEX IF EXISTS idx_nda_notification;
DROP TABLE IF EXISTS notification_delivery_attempts;
