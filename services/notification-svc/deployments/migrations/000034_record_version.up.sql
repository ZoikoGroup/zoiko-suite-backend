-- 000034_record_version.up.sql
--
-- Add optimistic-concurrency version (record_version) to mutable aggregates
-- in notification-svc. API-3 requires ETag/If-Match on every aggregate
-- mutation; GET must return ETag, and mutations require If-Match.

ALTER TABLE notifications
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE communication_intents
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE intent_versions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE template_definitions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE template_versions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE notifications
    ADD CONSTRAINT notification_version_positive CHECK (record_version > 0);

ALTER TABLE communication_intents
    ADD CONSTRAINT intent_version_positive CHECK (record_version > 0);

ALTER TABLE intent_versions
    ADD CONSTRAINT intent_ver_version_positive CHECK (record_version > 0);

ALTER TABLE template_definitions
    ADD CONSTRAINT tdef_version_positive CHECK (record_version > 0);

ALTER TABLE template_versions
    ADD CONSTRAINT tvar_version_positive CHECK (record_version > 0);