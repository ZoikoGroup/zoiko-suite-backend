-- +migrate Up
-- Contract activation previously required only a signature (signed_by),
-- with no reference to any approving governance decision at all — a
-- contract could go ACTIVE without any recorded authorization. This column
-- records the governance-decision-log-svc decision that authorized
-- activation; the application layer (handler.ActivateContract) verifies
-- the decision is GRANTED before allowing the write, and populates this
-- column from the caller-supplied decision ID.
BEGIN;

ALTER TABLE contracts
    ADD COLUMN IF NOT EXISTS governance_decision_id TEXT;

COMMIT;
