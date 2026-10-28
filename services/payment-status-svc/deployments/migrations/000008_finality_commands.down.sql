-- Migration 000008 down. Refused while any row depends on the mechanism:
-- dropping the evidence or claim would forget a finality decision or allow a
-- retried command to apply twice; dropping the state basis would leave a
-- payment claiming manual finality with no evidence. Manual rollback only.
--
-- The checks must see every tenant's rows, not only the migration role's own
-- tenant scope. payment_finality_evidence, payment_command_idempotency and
-- payment_execution_states are all FORCE ROW LEVEL SECURITY, so a role
-- subject to row security would otherwise see only rows visible under its
-- current app.tenant_id and could pass on an empty view while real,
-- differently-scoped data still exists. A role that cannot disable row
-- security fails the query instead, which is still a refusal, never a
-- silent loss.
SET LOCAL row_security = off;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM payment_finality_evidence) THEN
        RAISE EXCEPTION 'cannot roll back 000008: payment finality evidence exists';
    END IF;
    IF EXISTS (SELECT 1 FROM payment_command_idempotency) THEN
        RAISE EXCEPTION 'cannot roll back 000008: payment command idempotency claims exist';
    END IF;
    IF EXISTS (SELECT 1 FROM payment_execution_states
                WHERE finality_source IN ('MANUAL_CONFIRMATION', 'MANUAL_REJECTION')) THEN
        RAISE EXCEPTION 'cannot roll back 000008: payments depend on manual finality state';
    END IF;
END $$;

DROP TABLE IF EXISTS payment_command_idempotency;
DROP TABLE IF EXISTS payment_finality_evidence;
