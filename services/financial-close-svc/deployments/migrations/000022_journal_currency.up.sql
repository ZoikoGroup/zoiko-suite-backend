-- Migration: 000022_journal_currency.up.sql
--
-- SPEC_DEVIATIONS.md A13. Every journal this service posts goes through
-- general-ledger-svc's CreateJournal, which requires ACC-03's caller-supplied
-- inputs: journal_type, transaction/posting dates and currency (ZS-SVC-B-001
-- ACC-03 "Required source inputs: entity/book; journal type; document/
-- transaction/posting dates; currency; ..."). Five records post journals and
-- none of them carried a currency, so there was nothing to send:
--
--   accrual_schedules      ACC-07 recognition
--   prepayment_schedules   ACC-08 recognition and termination settlement
--   allocation_rules       ACC-09 allocation (per rule version)
--   fx_revaluation_runs    ACC-10 revaluation adjustment (the book currency
--                          the adjustment is booked in, not an item's
--                          foreign currency)
--   migration_batches      ACC-17 opening balances
--
-- currency_code is service-local because no authoritative source exists yet:
-- there is no REF-02 Currency Registry and no functional currency on any
-- legal entity anywhere in the platform. Reconcile it against the registry
-- once REF-02 ships.
--
-- Nullable, because rows written before this migration have no currency and
-- there is nothing to derive one from. Two triggers hold the line instead:
--
--   * INSERT: a new row must carry a well-formed ISO 4217 code. The API
--     already requires it; this also catches an internal insert that forgets
--     to copy it (allocation supersede, FX reversal).
--   * UPDATE: a currency, once set, never changes — journals already posted
--     in it would otherwise disagree with the record that produced them. A
--     legacy NULL may be set exactly once; until then the record cannot post
--     (the handlers refuse it with currency_code_missing before calling GL).

ALTER TABLE accrual_schedules    ADD COLUMN currency_code CHAR(3);
ALTER TABLE prepayment_schedules ADD COLUMN currency_code CHAR(3);
ALTER TABLE allocation_rules     ADD COLUMN currency_code CHAR(3);
ALTER TABLE fx_revaluation_runs  ADD COLUMN currency_code CHAR(3);
ALTER TABLE migration_batches    ADD COLUMN currency_code CHAR(3);

ALTER TABLE accrual_schedules    ADD CONSTRAINT accrual_schedules_currency_format    CHECK (currency_code ~ '^[A-Z]{3}$');
ALTER TABLE prepayment_schedules ADD CONSTRAINT prepayment_schedules_currency_format CHECK (currency_code ~ '^[A-Z]{3}$');
ALTER TABLE allocation_rules     ADD CONSTRAINT allocation_rules_currency_format     CHECK (currency_code ~ '^[A-Z]{3}$');
ALTER TABLE fx_revaluation_runs  ADD CONSTRAINT fx_revaluation_runs_currency_format  CHECK (currency_code ~ '^[A-Z]{3}$');
ALTER TABLE migration_batches    ADD CONSTRAINT migration_batches_currency_format    CHECK (currency_code ~ '^[A-Z]{3}$');

CREATE OR REPLACE FUNCTION guard_journal_currency() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.currency_code IS NULL THEN
            RAISE EXCEPTION 'currency_code is required on every new % row', TG_TABLE_NAME
                USING ERRCODE = '23502', CONSTRAINT = 'journal_currency_required';
        END IF;
    ELSIF OLD.currency_code IS NOT NULL AND NEW.currency_code IS DISTINCT FROM OLD.currency_code THEN
        RAISE EXCEPTION 'currency_code of a % row is immutable once set', TG_TABLE_NAME
            USING ERRCODE = '23514', CONSTRAINT = 'journal_currency_immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_accrual_schedules_currency
    BEFORE INSERT OR UPDATE OF currency_code ON accrual_schedules
    FOR EACH ROW EXECUTE FUNCTION guard_journal_currency();
CREATE TRIGGER trg_prepayment_schedules_currency
    BEFORE INSERT OR UPDATE OF currency_code ON prepayment_schedules
    FOR EACH ROW EXECUTE FUNCTION guard_journal_currency();
CREATE TRIGGER trg_allocation_rules_currency
    BEFORE INSERT OR UPDATE OF currency_code ON allocation_rules
    FOR EACH ROW EXECUTE FUNCTION guard_journal_currency();
CREATE TRIGGER trg_fx_revaluation_runs_currency
    BEFORE INSERT OR UPDATE OF currency_code ON fx_revaluation_runs
    FOR EACH ROW EXECUTE FUNCTION guard_journal_currency();
CREATE TRIGGER trg_migration_batches_currency
    BEFORE INSERT OR UPDATE OF currency_code ON migration_batches
    FOR EACH ROW EXECUTE FUNCTION guard_journal_currency();
