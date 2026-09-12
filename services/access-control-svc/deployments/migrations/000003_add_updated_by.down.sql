-- Corresponding rollback for 000003.

ALTER TABLE permission_bundle_defs DROP COLUMN updated_by_principal_id;
ALTER TABLE role_definitions DROP COLUMN updated_by_principal_id;