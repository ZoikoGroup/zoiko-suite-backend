-- 000005_add_capability_claim_approval_workflow.down.sql
ALTER TABLE capability_claims
    DROP CONSTRAINT IF EXISTS capability_claims_approval_consistency;
ALTER TABLE capability_claims
    DROP CONSTRAINT IF EXISTS capability_claims_approval_status_valid;
ALTER TABLE capability_claims
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS approval_status;
ALTER TABLE capability_claims
    ALTER COLUMN approved_by_principal_id SET NOT NULL;
