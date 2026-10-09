-- Migration: 000011_block_truncate.down.sql
-- Revert TRUNCATE blocking triggers.

DROP TRIGGER IF EXISTS governance_decisions_no_truncate ON governance_decisions;
DROP TRIGGER IF EXISTS replay_manifests_no_truncate ON replay_manifests;
DROP FUNCTION IF EXISTS reject_truncate();