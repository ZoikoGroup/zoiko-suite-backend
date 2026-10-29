-- Migration: 000005_add_command_idempotency.down.sql
--
-- Manual rollback only, and refused while any claim is recorded. Dropping the
-- table would forget that a key already moved a destination: a client
-- retrying the command after the rollback would then get a 422 instead of
-- the replay it expects, or worse, re-execute the command. Same posture as
-- every other command_idempotency down migration in this domain
-- (intercompany-accounting-svc 000009, consolidation-svc 000008,
-- financial-control-svc 000007).

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM command_idempotency) THEN
        RAISE EXCEPTION 'cannot roll back 000005: command idempotency claims exist';
    END IF;
END $$;

DROP TABLE IF EXISTS command_idempotency;
