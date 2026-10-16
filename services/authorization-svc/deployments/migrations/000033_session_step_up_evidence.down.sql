-- Reverts 000033. Refuses while any session carries step-up evidence: the
-- columns are review evidence for privileged access (§13/§14), and dropping
-- them would erase it.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM privileged_sessions
               WHERE requester_assurance IS NOT NULL OR approver_assurance IS NOT NULL)
       OR EXISTS (SELECT 1 FROM break_glass_sessions WHERE requester_assurance IS NOT NULL) THEN
        RAISE EXCEPTION '000033 down refused: sessions carry step-up evidence';
    END IF;
END $$;

ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_step_up_check;
ALTER TABLE privileged_sessions DROP CONSTRAINT IF EXISTS privileged_sessions_step_up_check;

ALTER TABLE break_glass_sessions
    DROP COLUMN IF EXISTS requester_identity_session,
    DROP COLUMN IF EXISTS requester_authn_at,
    DROP COLUMN IF EXISTS requester_assurance;

ALTER TABLE privileged_sessions
    DROP COLUMN IF EXISTS approver_identity_session,
    DROP COLUMN IF EXISTS approver_authn_at,
    DROP COLUMN IF EXISTS approver_assurance,
    DROP COLUMN IF EXISTS requester_identity_session,
    DROP COLUMN IF EXISTS requester_authn_at,
    DROP COLUMN IF EXISTS requester_assurance;
