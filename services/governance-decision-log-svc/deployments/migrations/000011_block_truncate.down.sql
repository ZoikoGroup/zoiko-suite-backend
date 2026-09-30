-- Migration: 000011_block_truncate.down.sql
-- Drops all objects created by 000011_block_truncate.up.sql.

DROP TRIGGER IF EXISTS outbox_block_truncate ON outbox;
DROP TRIGGER IF EXISTS idempotency_keys_block_truncate ON idempotency_keys;
DROP TRIGGER IF EXISTS replay_manifests_block_truncate ON replay_manifests;
DROP TRIGGER IF EXISTS governance_decisions_block_truncate ON governance_decisions;
DROP FUNCTION IF EXISTS block_truncate();