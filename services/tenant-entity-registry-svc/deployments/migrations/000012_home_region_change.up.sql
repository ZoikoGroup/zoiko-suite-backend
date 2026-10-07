-- 000012_home_region_change.up.sql
--
-- ORG-02 §4.2 ChangeHomeRegion.
--
-- §4.2 names TenantHomeRegionReference among this service's authoritative
-- facts and requires "home-region changes require maker-checker", but no
-- command existed, so the control could not be exercised (23 Sep audit ❌,
-- set aside until 28 Sep 2026). The home region is the region of the tenant's
-- default residency policy. The command re-points it under platform authority
-- and independent approval, and records the home-region decision — §4.2's
-- required evidence — on the lineage row.

ALTER TABLE approval_requests DROP CONSTRAINT ar_subject_known;
ALTER TABLE approval_requests ADD CONSTRAINT ar_subject_known CHECK (subject_type IN (
    'TENANT_CREATION', 'TENANT_COMMAND', 'LEGAL_PROFILE_AMENDMENT',
    'REGISTRY_CONFLICT_RESOLUTION', 'LEGAL_ENTITY_VERIFICATION',
    'LEGAL_ENTITY_MERGE', 'LEGAL_ENTITY_UNMERGE', 'TENANT_HOME_REGION'));

ALTER TABLE tenants
    ADD COLUMN home_region_decision_ref VARCHAR(255),
    ADD COLUMN home_region_changed_at   TIMESTAMP WITH TIME ZONE;

ALTER TABLE tenant_lifecycle_history
    ADD COLUMN home_region_decision_ref VARCHAR(255),
    ADD COLUMN from_region_id           UUID,
    ADD COLUMN to_region_id             UUID;

-- A home-region change without its decision reference is not evidence.
ALTER TABLE tenant_lifecycle_history
    ADD CONSTRAINT tlh_home_region_evidenced CHECK (
        command_name <> 'ChangeHomeRegion'
        OR (home_region_decision_ref IS NOT NULL AND to_region_id IS NOT NULL
            AND approved_by_principal_id IS NOT NULL));
