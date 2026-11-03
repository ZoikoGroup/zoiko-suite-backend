ALTER TABLE public_ai.conflicts
    DROP COLUMN IF EXISTS resolution_evidence_ref,
    DROP COLUMN IF EXISTS assigned_owner,
    DROP COLUMN IF EXISTS blocked_claim;
