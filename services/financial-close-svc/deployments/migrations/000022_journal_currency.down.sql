-- Migration: 000022_journal_currency.down.sql
--
-- Refused while any row on any of the five tables carries a currency_code.
-- A record that has already posted a journal in that currency has no other
-- copy of it once this column is gone — general-ledger-svc's own journal
-- still knows, but this service would not, and a currency is immutable once
-- set precisely because nothing can re-derive it afterward. Same posture as
-- every other irreplaceable-data down migration in this domain.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM accrual_schedules WHERE currency_code IS NOT NULL)
        OR EXISTS (SELECT 1 FROM prepayment_schedules WHERE currency_code IS NOT NULL)
        OR EXISTS (SELECT 1 FROM allocation_rules WHERE currency_code IS NOT NULL)
        OR EXISTS (SELECT 1 FROM fx_revaluation_runs WHERE currency_code IS NOT NULL)
        OR EXISTS (SELECT 1 FROM migration_batches WHERE currency_code IS NOT NULL)
    THEN
        RAISE EXCEPTION 'cannot roll back 000022: at least one record already carries a currency_code';
    END IF;
END $$;

DROP TRIGGER IF EXISTS trg_migration_batches_currency    ON migration_batches;
DROP TRIGGER IF EXISTS trg_fx_revaluation_runs_currency  ON fx_revaluation_runs;
DROP TRIGGER IF EXISTS trg_allocation_rules_currency     ON allocation_rules;
DROP TRIGGER IF EXISTS trg_prepayment_schedules_currency ON prepayment_schedules;
DROP TRIGGER IF EXISTS trg_accrual_schedules_currency    ON accrual_schedules;
DROP FUNCTION IF EXISTS guard_journal_currency();

ALTER TABLE migration_batches    DROP COLUMN IF EXISTS currency_code;
ALTER TABLE fx_revaluation_runs  DROP COLUMN IF EXISTS currency_code;
ALTER TABLE allocation_rules     DROP COLUMN IF EXISTS currency_code;
ALTER TABLE prepayment_schedules DROP COLUMN IF EXISTS currency_code;
ALTER TABLE accrual_schedules    DROP COLUMN IF EXISTS currency_code;
