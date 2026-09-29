-- 000005_decision_conditions_and_expiry.up.sql
-- Add conditions and expires_at to transfer_decisions per §16.1 & §18.

ALTER TABLE transfer_decisions
    ADD COLUMN IF NOT EXISTS conditions TEXT,
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
