-- 000034_record_version.down.sql
--
-- Revert record_version additions for mutable aggregates.

ALTER TABLE template_versions
    DROP CONSTRAINT IF EXISTS tvar_version_positive;
ALTER TABLE template_versions
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE template_definitions
    DROP CONSTRAINT IF EXISTS tdef_version_positive;
ALTER TABLE template_definitions
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE intent_versions
    DROP CONSTRAINT IF EXISTS intent_ver_version_positive;
ALTER TABLE intent_versions
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE communication_intents
    ADD CONSTRAINT IF EXISTS intent_version_positive;
ALTER TABLE communication_intents
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notification_version_positive;
ALTER TABLE notifications
    DROP COLUMN IF EXISTS record_version;