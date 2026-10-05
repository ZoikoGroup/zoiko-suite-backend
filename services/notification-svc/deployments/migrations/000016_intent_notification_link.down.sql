-- 000016 down: removes the intent/notification link. Neither table loses any other data.
DROP TRIGGER IF EXISTS trg_notifications_link_guard ON notifications;
DROP TRIGGER IF EXISTS trg_message_intents_link_guard ON message_intents;
DROP FUNCTION IF EXISTS guard_intent_notification_link();
DROP INDEX IF EXISTS uq_notifications_message_intent;
DROP INDEX IF EXISTS uq_message_intents_notification;
ALTER TABLE notifications DROP COLUMN IF EXISTS message_intent_id;
ALTER TABLE message_intents DROP COLUMN IF EXISTS notification_id;
