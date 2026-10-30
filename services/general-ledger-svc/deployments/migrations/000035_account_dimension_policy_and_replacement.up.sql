-- Global Accounting Kernel (standard §12): an account states which dimensions its lines must, may or must not carry, and a retired
-- account can name the account it maps forward to. Neither rewrites a posted line: history keeps the account it was posted to.
ALTER TABLE chart_of_accounts
    ADD COLUMN dimension_policy          JSONB,
    ADD COLUMN replaced_by_account_code  VARCHAR(64);

ALTER TABLE chart_of_accounts
    ADD CONSTRAINT chart_of_accounts_dimension_policy_shape CHECK (dimension_policy IS NULL OR jsonb_typeof(dimension_policy) = 'object'),
    ADD CONSTRAINT chart_of_accounts_replacement_not_self CHECK (replaced_by_account_code IS NULL OR replaced_by_account_code <> account_code);
