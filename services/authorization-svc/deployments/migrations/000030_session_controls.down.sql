-- Reverts 000030. Refused while any JIT session is PENDING_APPROVAL or REJECTED,
-- which the 000014 status CHECK cannot hold.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM privileged_sessions WHERE status IN ('PENDING_APPROVAL', 'REJECTED')) THEN
        RAISE EXCEPTION '000030 down refused: privileged_sessions has PENDING_APPROVAL or REJECTED rows';
    END IF;
END
$$;

DROP TRIGGER IF EXISTS privileged_sessions_events ON privileged_sessions;
DROP TRIGGER IF EXISTS break_glass_sessions_events ON break_glass_sessions;
DROP TRIGGER IF EXISTS support_sessions_events ON support_sessions;
DROP FUNCTION IF EXISTS authz_session_events();
DROP TRIGGER IF EXISTS privileged_sessions_review_on_end ON privileged_sessions;
DROP TRIGGER IF EXISTS break_glass_sessions_review_on_end ON break_glass_sessions;
DROP FUNCTION IF EXISTS authz_session_review_on_end();

ALTER TABLE privileged_sessions DROP CONSTRAINT IF EXISTS privileged_sessions_independent_review;
ALTER TABLE privileged_sessions DROP CONSTRAINT IF EXISTS privileged_sessions_independent_approval;
ALTER TABLE privileged_sessions DROP CONSTRAINT IF EXISTS privileged_sessions_review_check;
ALTER TABLE privileged_sessions DROP CONSTRAINT IF EXISTS privileged_sessions_status_check;
ALTER TABLE privileged_sessions ADD CONSTRAINT privileged_sessions_status_check
    CHECK (status IN ('ACTIVE', 'REVOKED', 'EXPIRED'));
ALTER TABLE privileged_sessions
    DROP COLUMN IF EXISTS requested_by, DROP COLUMN IF EXISTS resource_scope,
    DROP COLUMN IF EXISTS target_environment, DROP COLUMN IF EXISTS high_impact,
    DROP COLUMN IF EXISTS approved_by, DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS review_status, DROP COLUMN IF EXISTS reviewed_by,
    DROP COLUMN IF EXISTS reviewed_at, DROP COLUMN IF EXISTS review_notes;

ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_independent_review;
ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_review_check;
ALTER TABLE break_glass_sessions
    DROP COLUMN IF EXISTS requested_by, DROP COLUMN IF EXISTS review_status,
    DROP COLUMN IF EXISTS reviewed_by, DROP COLUMN IF EXISTS reviewed_at,
    DROP COLUMN IF EXISTS review_notes;

ALTER TABLE support_sessions DROP COLUMN IF EXISTS requested_by;
