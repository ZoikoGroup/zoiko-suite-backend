-- 000006_add_release_reconciliation_procedure_ref.down.sql
ALTER TABLE releases
    DROP CONSTRAINT IF EXISTS releases_emergency_requires_reconciliation;
ALTER TABLE releases
    DROP COLUMN IF EXISTS reconciliation_procedure_ref;
