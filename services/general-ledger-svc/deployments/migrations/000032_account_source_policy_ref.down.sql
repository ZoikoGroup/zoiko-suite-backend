ALTER TABLE account_change_approvals DROP COLUMN IF EXISTS source_policy_ref;
ALTER TABLE account_versions DROP COLUMN IF EXISTS source_policy_ref;
