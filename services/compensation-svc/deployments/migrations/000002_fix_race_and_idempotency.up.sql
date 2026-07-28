-- 000002_fix_race_and_idempotency.up.sql
--
-- 1. A partial unique index enforcing "at most one ACTIVE wage revision
--    per employee" at the database level. Without this, two concurrent
--    ReviseWage calls for the same employee could both pass the
--    application-level supersede-then-insert sequence and leave two rows
--    with status='ACTIVE' — GetActiveWageRevision (LIMIT 1, no ORDER BY)
--    would then return an arbitrary one of them to payroll.
CREATE UNIQUE INDEX idx_wage_revisions_one_active
    ON wage_revisions (tenant_id, employee_id)
    WHERE status = 'ACTIVE';

-- 2. correlation_id columns + partial unique indexes so a retried create
--    call resolves to the ORIGINAL row instead of creating a duplicate
--    compensation structure, wage revision, or bonus grant.
ALTER TABLE compensation_structures ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_comp_struct_tenant_correlation
    ON compensation_structures (tenant_id, correlation_id)
    WHERE correlation_id != '';

ALTER TABLE wage_revisions ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_wage_rev_tenant_correlation
    ON wage_revisions (tenant_id, correlation_id)
    WHERE correlation_id != '';

ALTER TABLE bonus_grants ADD COLUMN correlation_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_bonus_grants_tenant_correlation
    ON bonus_grants (tenant_id, correlation_id)
    WHERE correlation_id != '';
