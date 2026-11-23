ALTER TABLE public_ai.control_audit
    DROP COLUMN IF EXISTS row_hash,
    DROP COLUMN IF EXISTS prev_hash,
    DROP COLUMN IF EXISTS actor_role,
    DROP COLUMN IF EXISTS request_id;
