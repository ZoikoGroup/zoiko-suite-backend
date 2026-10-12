DROP TABLE IF EXISTS idempotency_keys;
DROP TRIGGER IF EXISTS trg_delegation_history_append_only ON delegation_history;
DROP FUNCTION IF EXISTS delegation_history_append_only();
-- delegation_history is kept: it is the record of who did what, and a
-- rollback of the schema does not un-happen those acts.
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_revocation_reason;
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_suspended_has_evidence;
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_delegator_approval_is_delegator;
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_approval_segregated;
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_approval_evidence;
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_approval_method_known;
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_status_known;
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_status_known CHECK (status IN ('ACTIVE', 'REVOKED', 'EXPIRED')) NOT VALID;
-- The evidence columns are kept for the same reason as the history.
