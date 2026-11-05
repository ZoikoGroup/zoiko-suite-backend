-- Migration 000010: add record_version for API-3 optimistic concurrency control
-- on mutable aggregates: evidence_requirements (end-date) and
-- evidence_requests (11 PBC mutation routes). The version increments on every
-- successful mutation; clients must send the current value in If-Match.

ALTER TABLE evidence_requirements ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;
CREATE INDEX idx_evidence_requirements_version ON evidence_requirements (record_version);

ALTER TABLE evidence_requests ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;
CREATE INDEX idx_evidence_requests_version ON evidence_requests (record_version);

-- Backfill: existing rows start at version 1 (already set by DEFAULT)