-- +migrate Down
BEGIN;

DROP INDEX IF EXISTS idx_meetings_tenant_scheduled;
DROP INDEX IF EXISTS idx_resolutions_tenant_created;
DROP INDEX IF EXISTS idx_resolutions_tenant_meeting;

ALTER TABLE board_resolutions DROP CONSTRAINT IF EXISTS board_resolutions_passed_is_attributed;
ALTER TABLE board_resolutions DROP CONSTRAINT IF EXISTS board_resolutions_category_known;
ALTER TABLE board_resolutions DROP CONSTRAINT IF EXISTS board_resolutions_status_known;
ALTER TABLE board_meetings DROP CONSTRAINT IF EXISTS board_meetings_status_known;
ALTER TABLE board_resolutions DROP CONSTRAINT IF EXISTS board_resolutions_votes_non_negative;

DROP POLICY IF EXISTS resolutions_tenant_isolation ON board_resolutions;
CREATE POLICY resolutions_tenant_isolation ON board_resolutions
    USING (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS meetings_tenant_isolation ON board_meetings;
CREATE POLICY meetings_tenant_isolation ON board_meetings
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE board_resolutions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE board_meetings NO FORCE ROW LEVEL SECURITY;

COMMIT;
