-- 000002_add_audit_columns.up.sql
-- Add audit update tracking columns to jurisdictions and jurisdiction_rules per R3 doctrine.

ALTER TABLE jurisdictions
    ADD COLUMN updated_at              TIMESTAMPTZ,
    ADD COLUMN updated_by_principal_id TEXT;

ALTER TABLE jurisdiction_rules
    ADD COLUMN updated_at              TIMESTAMPTZ,
    ADD COLUMN updated_by_principal_id TEXT;

-- Add idempotent creation unique index on jurisdiction_rules per dedup requirement (jurisdiction_id + rule_code + effective_from)
CREATE UNIQUE INDEX idx_jrules_jurisdiction_code_effective_from_unique
    ON jurisdiction_rules (jurisdiction_id, rule_code, effective_from);
