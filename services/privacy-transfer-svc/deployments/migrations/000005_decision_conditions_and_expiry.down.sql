-- 000005_decision_conditions_and_expiry.down.sql

ALTER TABLE transfer_decisions
    DROP COLUMN IF EXISTS conditions,
    DROP COLUMN IF EXISTS expires_at;
