ALTER TABLE policy_versions DROP CONSTRAINT IF EXISTS chk_policy_versions_scope_type;
ALTER TABLE policy_versions DROP COLUMN IF EXISTS scope_type;
