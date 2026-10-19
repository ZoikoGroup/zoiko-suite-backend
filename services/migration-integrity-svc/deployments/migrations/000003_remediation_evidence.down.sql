-- Migration: 000003_remediation_evidence.down.sql
--
-- Reverts the remediation-evidence columns.

ALTER TABLE migration_audit_entries
    DROP COLUMN IF EXISTS remediation_reason,
    DROP COLUMN IF EXISTS remediation_evidence_ref,
    DROP COLUMN IF EXISTS remediated_by,
    DROP COLUMN IF EXISTS remediated_at;
