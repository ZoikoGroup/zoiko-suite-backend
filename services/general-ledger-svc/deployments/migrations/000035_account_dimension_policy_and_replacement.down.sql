ALTER TABLE chart_of_accounts
    DROP CONSTRAINT IF EXISTS chart_of_accounts_replacement_not_self,
    DROP CONSTRAINT IF EXISTS chart_of_accounts_dimension_policy_shape,
    DROP COLUMN IF EXISTS replaced_by_account_code,
    DROP COLUMN IF EXISTS dimension_policy;
