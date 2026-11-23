-- SPEC_DEVIATIONS A11: ApproveMigrationBatch and CertifyMigrationBatch now
-- claim an Idempotency-Key, the same house table and rule 000021 (B3) built
-- for the four aggregate creates and later passes (B10-B13) extended to the
-- ACC-07/08/09/10 lifecycle transitions. migration_batches has no column of
-- its own yet in command_idempotency.

ALTER TABLE command_idempotency
    ADD COLUMN migration_batch_id UUID REFERENCES migration_batches(batch_id);

ALTER TABLE command_idempotency
    DROP CONSTRAINT chk_command_idempotency_one_target;
ALTER TABLE command_idempotency
    ADD CONSTRAINT chk_command_idempotency_one_target CHECK (
        num_nonnulls(accrual_schedule_id, prepayment_schedule_id,
                     allocation_rule_version_id, fx_revaluation_run_id,
                     migration_batch_id) = 1
    );

CREATE INDEX idx_command_idempotency_migration ON command_idempotency (tenant_id, migration_batch_id);
