-- Migration: 000008_add_command_idempotency.down.sql
--
-- Manual rollback only, and refused while any claim is recorded. Dropping the
-- table would forget that a key already created a run or moved an adjustment:
-- a client retrying the command after the rollback would then create a second
-- run or post/reverse a second time. Same posture as 000006 and 000007.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM command_idempotency) THEN
        RAISE EXCEPTION 'cannot roll back 000008: command idempotency claims exist';
    END IF;
END $$;

DROP TABLE IF EXISTS command_idempotency;
