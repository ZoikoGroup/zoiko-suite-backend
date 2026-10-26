-- Migration 000033: step-up evidence on privileged and break-glass sessions
-- (Governance audit register A-03 / S-3; ZS-IAM-001 §13 "eligible, requested,
-- step-up authenticated, time-bound and reviewed", §14 "high-assurance
-- step-up; compromised/unverified session cannot break glass").
--
-- Issuing or approving a JIT session, and issuing a break-glass session, now
-- requires the caller's verified identity envelope to carry a recent MFA
-- authentication (internal/handler: requireSessionStepUp). These columns
-- record what was proven, so the §13/§14 retrospective review can see it:
--
--   *_assurance           MFA (the only level that passes today) — or
--                         SINGLE_FACTOR, written only when a local stack runs
--                         with AUTHZ_SESSION_STEP_UP=off
--   *_authn_at            when the person authenticated (the envelope's
--                         auth_time), not when the row was written
--   *_identity_session    identity-context-svc's session_context_id
--
-- Rows from before this migration have NULLs: they were issued without a
-- step-up, and NULL says so rather than inventing one.

ALTER TABLE privileged_sessions
    ADD COLUMN IF NOT EXISTS requester_assurance        VARCHAR(20),
    ADD COLUMN IF NOT EXISTS requester_authn_at         TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS requester_identity_session VARCHAR(64),
    ADD COLUMN IF NOT EXISTS approver_assurance         VARCHAR(20),
    ADD COLUMN IF NOT EXISTS approver_authn_at          TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS approver_identity_session  VARCHAR(64);

ALTER TABLE break_glass_sessions
    ADD COLUMN IF NOT EXISTS requester_assurance        VARCHAR(20),
    ADD COLUMN IF NOT EXISTS requester_authn_at         TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS requester_identity_session VARCHAR(64);

ALTER TABLE privileged_sessions DROP CONSTRAINT IF EXISTS privileged_sessions_step_up_check;
ALTER TABLE privileged_sessions ADD CONSTRAINT privileged_sessions_step_up_check CHECK (
    (requester_assurance IS NULL OR requester_assurance IN ('MFA', 'SINGLE_FACTOR'))
    AND (approver_assurance IS NULL OR approver_assurance IN ('MFA', 'SINGLE_FACTOR'))
    -- An assurance level is meaningless without the time it was proven.
    AND ((requester_assurance IS NULL) = (requester_authn_at IS NULL))
    AND ((approver_assurance IS NULL) = (approver_authn_at IS NULL))
);

ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_step_up_check;
ALTER TABLE break_glass_sessions ADD CONSTRAINT break_glass_sessions_step_up_check CHECK (
    (requester_assurance IS NULL OR requester_assurance IN ('MFA', 'SINGLE_FACTOR'))
    AND ((requester_assurance IS NULL) = (requester_authn_at IS NULL))
);
