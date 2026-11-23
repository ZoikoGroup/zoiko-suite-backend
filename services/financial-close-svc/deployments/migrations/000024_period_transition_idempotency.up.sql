-- SPEC_DEVIATIONS A11: StartSoftClose and EnterCloseReview now claim an
-- Idempotency-Key, the same house table and rule 000021 (B3) built for the
-- four aggregate creates and later passes (B10-B14) extended across the
-- ACC-07/08/09/10/17 lifecycle commands. fiscal_periods has no column of
-- its own yet in command_idempotency.
--
-- Scope: only the two simple, single-UPDATE period transitions this pass
-- closes (StartSoftClose, EnterCloseReview). HardClosePeriod/ReclosePeriod
-- and the reopen request/approve/reject commands are NOT claimed here — see
-- SPEC_DEVIATIONS for why (readiness checks, kernel sealing and evidence
-- signing make their idempotency boundary a separate, larger design
-- question, not a drop-in claim).

ALTER TABLE command_idempotency
    ADD COLUMN fiscal_period_id UUID REFERENCES fiscal_periods(fiscal_period_id);

ALTER TABLE command_idempotency
    DROP CONSTRAINT chk_command_idempotency_one_target;
ALTER TABLE command_idempotency
    ADD CONSTRAINT chk_command_idempotency_one_target CHECK (
        num_nonnulls(accrual_schedule_id, prepayment_schedule_id,
                     allocation_rule_version_id, fx_revaluation_run_id,
                     migration_batch_id, fiscal_period_id) = 1
    );

CREATE INDEX idx_command_idempotency_period ON command_idempotency (tenant_id, fiscal_period_id);
