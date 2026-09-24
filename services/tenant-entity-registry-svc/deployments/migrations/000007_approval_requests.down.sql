-- 000007_approval_requests.down.sql
--
-- Reverses 000007. The approval chain — who proposed and who approved each
-- governed change — is dropped with it; the approved_by_principal_id values
-- already copied onto lifecycle history and profile versions survive.

ALTER TABLE entity_registry_conflicts DROP CONSTRAINT IF EXISTS erc_no_self_approval;
ALTER TABLE entity_registry_conflicts
    DROP COLUMN IF EXISTS approval_request_id,
    DROP COLUMN IF EXISTS approved_by_principal_id;

ALTER TABLE legal_entity_profile_versions DROP CONSTRAINT IF EXISTS lepv_no_self_approval;
ALTER TABLE legal_entity_profile_versions DROP COLUMN IF EXISTS approval_request_id;
ALTER TABLE tenant_lifecycle_history DROP COLUMN IF EXISTS approval_request_id;

DROP TABLE IF EXISTS approval_requests;
