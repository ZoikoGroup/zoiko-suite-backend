DROP TABLE IF EXISTS run_instruction_payables;
DROP FUNCTION IF EXISTS reject_run_instruction_payable_mutation();

ALTER TABLE instruction_reconciliation_events DROP COLUMN IF EXISTS reason;
ALTER TABLE instruction_reconciliation_events
    DROP CONSTRAINT IF EXISTS instruction_reconciliation_events_external_status_check;
ALTER TABLE instruction_reconciliation_events
    ADD CONSTRAINT instruction_reconciliation_events_external_status_check
    CHECK (external_status IN ('ACCEPTED', 'REJECTED', 'SETTLED', 'EXCEPTION'));

ALTER TABLE payment_runs DROP CONSTRAINT IF EXISTS payment_runs_status_check;
ALTER TABLE payment_runs ADD CONSTRAINT payment_runs_status_check
    CHECK (status IN ('DRAFT', 'VALIDATED', 'LOCKED', 'SUBMITTED', 'ACCEPTED',
                      'REJECTED', 'PARTIALLY_ACCEPTED', 'SETTLED', 'COMPLETED',
                      'EXCEPTION', 'CANCELLED'));

ALTER TABLE run_instructions DROP COLUMN IF EXISTS status_reason;
ALTER TABLE run_instructions DROP CONSTRAINT IF EXISTS run_instructions_status_check;
ALTER TABLE run_instructions ADD CONSTRAINT run_instructions_status_check
    CHECK (status IN ('PENDING', 'ACCEPTED', 'REJECTED', 'SETTLED', 'EXCEPTION'));

DROP INDEX IF EXISTS uq_run_instructions_authorization_payee;
CREATE UNIQUE INDEX uq_run_instructions_authorization ON run_instructions (authorization_id);
