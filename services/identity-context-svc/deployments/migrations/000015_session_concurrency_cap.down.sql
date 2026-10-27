-- Migration: 000015_session_concurrency_cap.down.sql

ALTER TABLE session_contexts DROP CONSTRAINT IF EXISTS session_contexts_invalidation_reason_check;
ALTER TABLE session_contexts
    ADD CONSTRAINT session_contexts_invalidation_reason_check
    CHECK (invalidation_reason IS NULL OR invalidation_reason IN
           ('LOGOUT', 'ADMIN_REVOKE', 'RISK_ESCALATION', 'DELEGATION_REVOKED'));
