-- Migration: 000015_session_concurrency_cap.up.sql
--
-- ZS-IAM-001 §15: SESSION_CONCURRENCY_CAP is a new session invalidation reason.
-- It records that a session was revoked because the principal exceeded the
-- configured MaxSessionsPerPrincipal cap and this was the oldest live session.
-- Without this row in the CHECK constraint the store write fails with a
-- constraint violation and the cap enforcement silently does nothing.

ALTER TABLE session_contexts DROP CONSTRAINT IF EXISTS session_contexts_invalidation_reason_check;
ALTER TABLE session_contexts
    ADD CONSTRAINT session_contexts_invalidation_reason_check
    CHECK (invalidation_reason IS NULL OR invalidation_reason IN
           ('LOGOUT', 'ADMIN_REVOKE', 'RISK_ESCALATION', 'DELEGATION_REVOKED', 'SESSION_CONCURRENCY_CAP'));
