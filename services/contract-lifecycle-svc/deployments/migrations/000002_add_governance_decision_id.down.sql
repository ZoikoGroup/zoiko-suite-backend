-- +migrate Down
BEGIN;

ALTER TABLE contracts
    DROP COLUMN IF EXISTS governance_decision_id;

COMMIT;
