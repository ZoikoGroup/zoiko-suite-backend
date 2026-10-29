-- 000014_record_version_drp_tib.down.sql
--
-- Revert record_version additions for DataResidencyPolicy and TaxIdentityBundle.

ALTER TABLE tax_identity_bundles
    DROP CONSTRAINT IF EXISTS tib_version_positive;
ALTER TABLE tax_identity_bundles
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE data_residency_policies
    DROP CONSTRAINT IF EXISTS drp_version_positive;
ALTER TABLE data_residency_policies
    DROP COLUMN IF EXISTS record_version;