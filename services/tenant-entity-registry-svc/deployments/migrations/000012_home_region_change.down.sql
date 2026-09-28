-- 000012_home_region_change.down.sql
--
-- Reverses 000012. Refuses while a home-region approval exists, since the
-- narrowed constraint could not be re-added over it.

ALTER TABLE tenant_lifecycle_history DROP CONSTRAINT IF EXISTS tlh_home_region_evidenced;
ALTER TABLE tenant_lifecycle_history
    DROP COLUMN IF EXISTS to_region_id,
    DROP COLUMN IF EXISTS from_region_id,
    DROP COLUMN IF EXISTS home_region_decision_ref;
ALTER TABLE tenants
    DROP COLUMN IF EXISTS home_region_changed_at,
    DROP COLUMN IF EXISTS home_region_decision_ref;

ALTER TABLE approval_requests DROP CONSTRAINT ar_subject_known;
ALTER TABLE approval_requests ADD CONSTRAINT ar_subject_known CHECK (subject_type IN (
    'TENANT_CREATION', 'TENANT_COMMAND', 'LEGAL_PROFILE_AMENDMENT',
    'REGISTRY_CONFLICT_RESOLUTION', 'LEGAL_ENTITY_VERIFICATION',
    'LEGAL_ENTITY_MERGE', 'LEGAL_ENTITY_UNMERGE'));
