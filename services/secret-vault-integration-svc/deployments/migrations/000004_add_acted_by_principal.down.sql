DROP INDEX IF EXISTS idx_secret_access_audit_log_actor;
ALTER TABLE secret_access_audit_log DROP COLUMN IF EXISTS acted_by_principal_id;
