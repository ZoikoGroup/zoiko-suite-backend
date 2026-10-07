-- 000019 down: removes the communication class from notifications.
DROP TRIGGER IF EXISTS trg_notifications_freeze_class ON notifications;
DROP FUNCTION IF EXISTS freeze_notification_communication_class();
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS ck_notifications_communication_class;
ALTER TABLE notifications DROP COLUMN IF EXISTS communication_class;
