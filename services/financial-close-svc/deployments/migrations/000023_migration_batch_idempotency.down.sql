DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM command_idempotency WHERE migration_batch_id IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back 000023: % claim(s) already recorded for a migration batch',
            (SELECT count(*) FROM command_idempotency WHERE migration_batch_id IS NOT NULL);
    END IF;
END $$;

DROP INDEX IF EXISTS idx_command_idempotency_migration;

ALTER TABLE command_idempotency
    DROP CONSTRAINT chk_command_idempotency_one_target;
ALTER TABLE command_idempotency
    ADD CONSTRAINT chk_command_idempotency_one_target CHECK (
        num_nonnulls(accrual_schedule_id, prepayment_schedule_id,
                     allocation_rule_version_id, fx_revaluation_run_id) = 1
    );

ALTER TABLE command_idempotency DROP COLUMN migration_batch_id;
