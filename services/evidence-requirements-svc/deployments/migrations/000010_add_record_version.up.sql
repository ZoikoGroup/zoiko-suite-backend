-- Migration 000010: record_version for API-3 optimistic concurrency.
--
-- The two mutable aggregates this service owns get a version that every
-- mutation bumps in the same statement:
--   * evidence_requirements: end-dating is the only mutation;
--   * evidence_requests (PBC): send, reassign, extend-due-date, clarify,
--     mark-satisfied and close require If-Match; submit-response and view
--     move the status too, so they bump the version as well.
-- The version is served as the ETag "v<n>". A command whose If-Match names an
-- older version is refused with 412; one with no If-Match is refused with 428.
--
-- No new table, so no new RLS policy: both tables already carry FORCE RLS
-- with the NULLIF(current_setting('app.tenant_id', true), '')::uuid policy
-- (000002, 000003, 000004). Existing rows start at version 1.

ALTER TABLE evidence_requirements
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1
    CONSTRAINT evidence_requirements_record_version_positive CHECK (record_version >= 1);

ALTER TABLE evidence_requests
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1
    CONSTRAINT evidence_requests_record_version_positive CHECK (record_version >= 1);
