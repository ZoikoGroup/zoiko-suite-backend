-- 000034_record_version.up.sql
--
-- Add optimistic-concurrency version (record_version) to mutable aggregates
-- in notification-svc. API-3 requires ETag/If-Match on every aggregate
-- mutation; GET must return ETag, and mutations require If-Match.
--
-- The intent versions table is communication_intent_versions (000020); an
-- earlier draft of this file named intent_versions, which does not exist, so
-- the migration could not apply on any database.
--
-- The version is bumped by a trigger on every UPDATE rather than by each
-- store statement: cancel, resend, resolve, the delivery worker, the template
-- lifecycle and retirement all update these rows, and a path that forgot to
-- bump would leave the ETag unchanged, so a stale If-Match would pass.

ALTER TABLE notifications
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE communication_intents
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE communication_intent_versions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE template_definitions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE template_versions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE notifications
    ADD CONSTRAINT notification_version_positive CHECK (record_version > 0);

ALTER TABLE communication_intents
    ADD CONSTRAINT intent_version_positive CHECK (record_version > 0);

ALTER TABLE communication_intent_versions
    ADD CONSTRAINT intent_ver_version_positive CHECK (record_version > 0);

ALTER TABLE template_definitions
    ADD CONSTRAINT tdef_version_positive CHECK (record_version > 0);

ALTER TABLE template_versions
    ADD CONSTRAINT tvar_version_positive CHECK (record_version > 0);

CREATE OR REPLACE FUNCTION bump_record_version() RETURNS trigger AS $$
BEGIN
    NEW.record_version := OLD.record_version + 1;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER notifications_bump_version
    BEFORE UPDATE ON notifications
    FOR EACH ROW EXECUTE FUNCTION bump_record_version();

CREATE TRIGGER communication_intents_bump_version
    BEFORE UPDATE ON communication_intents
    FOR EACH ROW EXECUTE FUNCTION bump_record_version();

CREATE TRIGGER communication_intent_versions_bump_version
    BEFORE UPDATE ON communication_intent_versions
    FOR EACH ROW EXECUTE FUNCTION bump_record_version();

CREATE TRIGGER template_definitions_bump_version
    BEFORE UPDATE ON template_definitions
    FOR EACH ROW EXECUTE FUNCTION bump_record_version();

CREATE TRIGGER template_versions_bump_version
    BEFORE UPDATE ON template_versions
    FOR EACH ROW EXECUTE FUNCTION bump_record_version();
