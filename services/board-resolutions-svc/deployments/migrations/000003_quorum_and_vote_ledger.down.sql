-- +migrate Down
BEGIN;

ALTER TABLE board_resolutions DROP CONSTRAINT IF EXISTS board_resolutions_open_has_quorum;
ALTER TABLE board_resolutions DROP COLUMN IF EXISTS superseded_by;
ALTER TABLE board_resolutions DROP COLUMN IF EXISTS voting_closed_at;
ALTER TABLE board_resolutions DROP COLUMN IF EXISTS voting_opened_at;
ALTER TABLE board_resolutions DROP COLUMN IF EXISTS quorum_threshold;

ALTER TABLE board_resolutions DROP CONSTRAINT IF EXISTS board_resolutions_status_known;
ALTER TABLE board_resolutions
    ADD CONSTRAINT board_resolutions_status_known
    CHECK (status IN ('PROPOSED', 'PASSED', 'REJECTED', 'RESCINDED')) NOT VALID;

DROP TABLE IF EXISTS board_resolution_votes;
DROP TABLE IF EXISTS board_resolution_voter_roster;

COMMIT;
