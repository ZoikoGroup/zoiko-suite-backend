-- 000011: a refusal reason for re-delegating authority that is itself only
-- delegated (ORG-06 "delegation can narrow or transmit authority but cannot
-- manufacture authority").
--
-- authorization-svc confers through a delegation only actions the delegator
-- holds through their own roles, so a second hop conferred nothing at decision
-- time while this service accepted and explained it as a chain. The create path
-- now refuses it, and the refusal is evidence like every other.
ALTER TABLE refused_escalations DROP CONSTRAINT IF EXISTS refused_escalations_reason_known;
ALTER TABLE refused_escalations
    ADD CONSTRAINT refused_escalations_reason_known
    CHECK (refusal_reason IN ('self_dealing', 'delegator_mismatch', 'delegator_lacks_authority', 'sod_conflict',
                              'overlap_conflict', 'invalid_window', 'no_create_grant', 'delegate_is_delegator',
                              'delegator_exceeds_limit', 'invalid_limit', 'approval_not_segregated',
                              'delegator_authority_delegated'));
