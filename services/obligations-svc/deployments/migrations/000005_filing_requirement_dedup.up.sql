-- 000005_filing_requirement_dedup.up.sql
-- Obligations Service — idempotent filing requirement creation
--
-- Adds a unique index on (tenant_id, obligation_id, filing_type, filing_authority, submission_channel)
-- to support idempotent creation of filing requirements.

BEGIN;

-- Idempotent creation key for filing requirements: same type/authority/channel under same obligation = same requirement.
CREATE UNIQUE INDEX IF NOT EXISTS idx_filing_requirements_tenant_obligation_type_authority_channel
    ON filing_requirements (tenant_id, obligation_id, filing_type, filing_authority, submission_channel);

COMMIT;