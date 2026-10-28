-- Migration: 000021_acc_controls_hardening.down.sql
--
-- Removes the four controls. Refused while any idempotency claim is
-- recorded: dropping the table would forget that a key already created a
-- schedule/rule/run, so a client retrying the command after the rollback
-- would create a second one instead of replaying the first. Same posture as
-- every other command_idempotency down migration in this domain
-- (consolidation-svc 000008, financial-control-svc's BeginExecution,
-- intercompany-accounting-svc 000009, payee-banking-identity-svc 000005).

DROP TRIGGER IF EXISTS fiscal_periods_reopen_requires_approval ON fiscal_periods;
DROP FUNCTION IF EXISTS require_approved_reopen();
DROP TRIGGER IF EXISTS fiscal_periods_guard_transition ON fiscal_periods;
DROP FUNCTION IF EXISTS guard_fiscal_period_transition();

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM command_idempotency) THEN
        RAISE EXCEPTION 'cannot roll back 000021: command idempotency claims exist';
    END IF;
END $$;

DROP TABLE IF EXISTS command_idempotency;
DROP FUNCTION IF EXISTS reject_command_idempotency_mutation();

DROP INDEX IF EXISTS fx_revaluation_runs_one_reversal;

ALTER TABLE migration_batches DROP CONSTRAINT IF EXISTS sod_migration_certifier_independent;
ALTER TABLE migration_batches DROP CONSTRAINT IF EXISTS sod_migration_approver_independent;
ALTER TABLE fx_revaluation_runs DROP CONSTRAINT IF EXISTS sod_fx_revaluation_approver_independent;
ALTER TABLE allocation_rules DROP CONSTRAINT IF EXISTS sod_allocation_rule_approver_independent;
ALTER TABLE prepayment_schedules DROP CONSTRAINT IF EXISTS sod_prepayment_approver_independent;
ALTER TABLE accrual_schedules DROP CONSTRAINT IF EXISTS sod_accrual_approver_independent;
