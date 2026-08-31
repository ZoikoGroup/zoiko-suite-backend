DROP TRIGGER IF EXISTS trg_reject_run_events_mutation ON run_events;
DROP TRIGGER IF EXISTS trg_reject_instruction_reconciliation_events_mutation ON instruction_reconciliation_events;
DROP TRIGGER IF EXISTS trg_reject_run_instruction_mutation ON run_instructions;
DROP TRIGGER IF EXISTS trg_reject_run_mutation ON payment_runs;
DROP FUNCTION IF EXISTS reject_evidence_mutation();
DROP FUNCTION IF EXISTS reject_run_instruction_mutation();
DROP FUNCTION IF EXISTS reject_run_mutation();
