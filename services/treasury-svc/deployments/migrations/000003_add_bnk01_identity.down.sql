DROP TRIGGER IF EXISTS trg_reject_bank_account_mutation ON bank_accounts;
DROP FUNCTION IF EXISTS reject_bank_account_mutation();
DROP TRIGGER IF EXISTS trg_reject_ownership_evidence_mutation ON bank_account_ownership_evidence;
DROP FUNCTION IF EXISTS reject_ownership_evidence_mutation();
DROP TABLE IF EXISTS bank_account_ownership_evidence;
DROP INDEX IF EXISTS idx_bank_accounts_tenant_correlation;
ALTER TABLE bank_accounts DROP CONSTRAINT IF EXISTS bank_accounts_status_valid;
ALTER TABLE bank_accounts
    DROP COLUMN IF EXISTS branch_ref,
    DROP COLUMN IF EXISTS country,
    DROP COLUMN IF EXISTS account_type,
    DROP COLUMN IF EXISTS requested_operational_use,
    DROP COLUMN IF EXISTS correlation_id,
    DROP COLUMN IF EXISTS created_by_principal_id,
    DROP COLUMN IF EXISTS suspend_reason,
    DROP COLUMN IF EXISTS close_reason,
    DROP COLUMN IF EXISTS token_version;
