-- Rollback of 000013_int10_provisioning.up.sql
DROP POLICY IF EXISTS federation_worker_scan ON federation_links;
DROP TABLE IF EXISTS federation_sync_exceptions;
DROP TABLE IF EXISTS federation_assignments;
DROP TABLE IF EXISTS federation_provisioning_events;
DROP TABLE IF EXISTS federation_mapping_versions;
DROP TABLE IF EXISTS federation_scim_tokens;
ALTER TABLE federation_links
    DROP COLUMN IF EXISTS deprovision_escalated_at,
    DROP COLUMN IF EXISTS access_revoked_at,
    DROP COLUMN IF EXISTS sessions_evicted_at,
    DROP COLUMN IF EXISTS deprovision_due_at,
    DROP COLUMN IF EXISTS deprovision_requested_at,
    DROP COLUMN IF EXISTS attributes_rev,
    DROP COLUMN IF EXISTS attributes,
    DROP COLUMN IF EXISTS username;
ALTER TABLE federation_connections DROP COLUMN IF EXISTS active_mapping_version;
DROP FUNCTION IF EXISTS federation_prov_guard();
DROP FUNCTION IF EXISTS federation_mapping_guard();
