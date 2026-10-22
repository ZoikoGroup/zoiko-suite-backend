-- Migration: 000008_command_idempotency_targets.down.sql
--
-- Manual rollback only. Claims that 000007's schema cannot hold (exception
-- transitions, and commands longer than 32 characters) are deleted: they are
-- retry protection, not records, so the cost is that a retry of one of those
-- commands made across the rollback is no longer recognised as a replay.

DELETE FROM command_idempotency WHERE exception_id IS NOT NULL OR length(command) > 32;
ALTER TABLE command_idempotency
    DROP COLUMN IF EXISTS exception_id,
    ALTER COLUMN command TYPE VARCHAR(32);
