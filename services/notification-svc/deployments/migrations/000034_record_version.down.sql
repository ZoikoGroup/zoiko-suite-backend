-- 000034_record_version.down.sql
--
-- Revert record_version additions for mutable aggregates.

DROP TRIGGER IF EXISTS template_versions_bump_version ON template_versions;
DROP TRIGGER IF EXISTS template_definitions_bump_version ON template_definitions;
DROP TRIGGER IF EXISTS communication_intent_versions_bump_version ON communication_intent_versions;
DROP TRIGGER IF EXISTS communication_intents_bump_version ON communication_intents;
DROP TRIGGER IF EXISTS notifications_bump_version ON notifications;
DROP FUNCTION IF EXISTS bump_record_version();

ALTER TABLE template_versions
    DROP CONSTRAINT IF EXISTS tvar_version_positive;
ALTER TABLE template_versions
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE template_definitions
    DROP CONSTRAINT IF EXISTS tdef_version_positive;
ALTER TABLE template_definitions
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE communication_intent_versions
    DROP CONSTRAINT IF EXISTS intent_ver_version_positive;
ALTER TABLE communication_intent_versions
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE communication_intents
    DROP CONSTRAINT IF EXISTS intent_version_positive;
ALTER TABLE communication_intents
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notification_version_positive;
ALTER TABLE notifications
    DROP COLUMN IF EXISTS record_version;
