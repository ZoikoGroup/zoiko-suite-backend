-- 000021 down: removes the intent binding of templates and the intent pin on notifications.
DROP TRIGGER IF EXISTS trg_notifications_freeze_intent_version ON notifications;
DROP FUNCTION IF EXISTS freeze_notification_intent_version();
DROP TRIGGER IF EXISTS trg_template_intent_binding ON template_definitions;
DROP FUNCTION IF EXISTS guard_template_intent_binding();
DROP INDEX IF EXISTS idx_template_definitions_intent;
ALTER TABLE notifications DROP COLUMN IF EXISTS intent_version_id;
ALTER TABLE template_definitions DROP COLUMN IF EXISTS intent_id;
