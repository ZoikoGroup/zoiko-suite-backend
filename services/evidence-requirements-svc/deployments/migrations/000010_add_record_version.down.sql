-- Migration 000010 down: remove the API-3 record_version columns.

ALTER TABLE evidence_requests DROP COLUMN IF EXISTS record_version;
ALTER TABLE evidence_requirements DROP COLUMN IF EXISTS record_version;
