-- 000014_record_version_drp_tib.up.sql
--
-- Add optimistic-concurrency version (record_version) to DataResidencyPolicy
-- and TaxIdentityBundle aggregates. API-3 requires ETag/If-Match on every
-- aggregate mutation; GET must return ETag, and mutations require If-Match.

ALTER TABLE data_residency_policies
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE tax_identity_bundles
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE data_residency_policies
    ADD CONSTRAINT drp_version_positive CHECK (record_version > 0);

ALTER TABLE tax_identity_bundles
    ADD CONSTRAINT tib_version_positive CHECK (record_version > 0);