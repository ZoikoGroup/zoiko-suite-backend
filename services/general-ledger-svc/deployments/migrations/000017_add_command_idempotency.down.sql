-- Migration: 000017_add_command_idempotency.down.sql
--
-- Drops only the replay-protection table; no ACC-01 data depends on it.

DROP TABLE IF EXISTS command_idempotency;
