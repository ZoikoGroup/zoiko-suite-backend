-- +migrate Down
BEGIN;

DROP INDEX IF EXISTS idx_resolutions_status;
DROP INDEX IF EXISTS idx_resolutions_meeting_id;
DROP INDEX IF EXISTS idx_resolutions_tenant_entity;
DROP INDEX IF EXISTS idx_meetings_tenant_entity;

DROP POLICY IF EXISTS resolutions_tenant_isolation ON board_resolutions;
DROP POLICY IF EXISTS meetings_tenant_isolation ON board_meetings;

DROP TABLE IF EXISTS board_resolutions;
DROP TABLE IF EXISTS board_meetings;

COMMIT;
