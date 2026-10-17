-- Migration: 000003_remediation_evidence.up.sql
--
-- Remediation evidence (ZS-MIG-001 MIG-INV-12 "owner, reason code and
-- disposition"; MIG-N27 "quarantine item manually edited without evidence →
-- reject resolution"; §24 "immutable issue history").
--
-- The remediate route used to accept a free-text note and discard it: nothing
-- was stored and nothing reached the migration.audit_entry_remediated event,
-- so a remediation was a boolean flip by anyone holding AUDIT_ENTRY_REMEDIATE
-- with no reason, no evidence reference and no record of who decided it or
-- when. These four columns are the evidence: a reason is mandatory per the
-- application (400 without it), the evidence reference names what supports
-- the resolution, and by/when record the accountable actor and time.
--
-- Written on the FIRST remediation only. The store's UPDATE coalesces onto
-- the existing values, so a replay cannot rewrite the recorded decision.

ALTER TABLE migration_audit_entries
    ADD COLUMN IF NOT EXISTS remediation_reason TEXT,
    ADD COLUMN IF NOT EXISTS remediation_evidence_ref TEXT,
    ADD COLUMN IF NOT EXISTS remediated_by VARCHAR(256),
    ADD COLUMN IF NOT EXISTS remediated_at TIMESTAMPTZ;
