-- Migration 000010 down: remove record_version columns

DROP INDEX IF EXISTS idx_evidence_requirements_version;
ALTER TABLE evidence_requirements DROP COLUMN IF EXISTS record_version;

DROP INDEX IF EXISTS idx_evidence_requests_version;
ALTER TABLE evidence_requests DROP COLUMN IF EXISTS record_version;