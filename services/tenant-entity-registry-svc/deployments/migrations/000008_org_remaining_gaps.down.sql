-- 000008_org_remaining_gaps.down.sql
--
-- Reverses 000008. Merge lineage, verification evidence, LEIs and onboarding
-- keys are dropped with their columns and tables. Any approval request of the
-- three new subject types must be removed first or the restored CHECK fails;
-- they are deleted here because the commands they governed no longer exist.
-- That delete runs LAST: entity_merge_records and
-- legal_entities.verification_approval_request_id reference those rows, so the
-- referencing table and column are dropped first.

DROP TABLE IF EXISTS entity_merge_records;

ALTER TABLE legal_entities
    DROP CONSTRAINT IF EXISTS le_not_merged_into_self,
    DROP CONSTRAINT IF EXISTS le_no_self_verification,
    DROP COLUMN IF EXISTS merged_at,
    DROP COLUMN IF EXISTS merged_into_legal_entity_id,
    DROP COLUMN IF EXISTS verification_approval_request_id,
    DROP COLUMN IF EXISTS verification_evidence_ref,
    DROP COLUMN IF EXISTS verified_at,
    DROP COLUMN IF EXISTS verified_by_principal_id;

ALTER TABLE legal_entity_profile_versions
    DROP CONSTRAINT IF EXISTS lepv_lei_status_known,
    DROP CONSTRAINT IF EXISTS lepv_lei_has_source_and_status,
    DROP CONSTRAINT IF EXISTS lepv_lei_format,
    DROP COLUMN IF EXISTS lei_verified_at,
    DROP COLUMN IF EXISTS lei_status,
    DROP COLUMN IF EXISTS lei_source,
    DROP COLUMN IF EXISTS lei;

DELETE FROM approval_requests
 WHERE subject_type IN ('LEGAL_ENTITY_VERIFICATION', 'LEGAL_ENTITY_MERGE', 'LEGAL_ENTITY_UNMERGE');
ALTER TABLE approval_requests DROP CONSTRAINT ar_subject_known;
ALTER TABLE approval_requests ADD CONSTRAINT ar_subject_known CHECK (subject_type IN (
    'TENANT_CREATION', 'TENANT_COMMAND',
    'LEGAL_PROFILE_AMENDMENT', 'REGISTRY_CONFLICT_RESOLUTION'));

DROP INDEX IF EXISTS tenants_external_customer_key_uq;
DROP TABLE IF EXISTS tenant_onboarding_keys;

ALTER TABLE tenants
    DROP COLUMN IF EXISTS provisioning_failed_at,
    DROP COLUMN IF EXISTS provisioning_failure_reason,
    DROP COLUMN IF EXISTS onboarding_request_ref,
    DROP COLUMN IF EXISTS external_customer_key;
