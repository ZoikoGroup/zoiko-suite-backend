-- Invariant #16, enforced by the database rather than only by a check that
-- two concurrent PrepareAttempt calls can both pass.
--
-- The pre-insert SELECT in PrepareAttempt is not atomic with the INSERT: for
-- one source_reference and two brand-new idempotency_keys, both callers can
-- observe "no unresolved attempt" and both insert. This partial unique index
-- makes the loser fail with a unique violation, which PrepareAttempt maps
-- back to the winner (ErrUnresolvedAttemptExists, HTTP 200 with the original
-- attempt) instead of creating a second live payment for one instruction.
--
-- The indexed statuses are exactly the unresolved-or-already-sent set the
-- application checks. REJECTED_BEFORE_SUBMISSION, CANCELLED and QUARANTINED
-- are deliberately outside it: once an attempt is resolved that way, a
-- genuine new instruction may reuse the source_reference.
--
-- tenant_id is nullable on legacy rows; COALESCE gives those rows one empty
-- tenant key so two NULL-tenant attempts still collide, matching the
-- application's RLS-visible check for a tenant-scoped caller.
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_initiation_attempts_unresolved_source_reference
    ON payment_initiation_attempts (COALESCE(tenant_id::text, ''), source_reference)
    WHERE source_reference <> ''
      AND status IN ('PREPARED', 'SUBMITTING', 'PENDING_UNKNOWN', 'SUBMITTED');
