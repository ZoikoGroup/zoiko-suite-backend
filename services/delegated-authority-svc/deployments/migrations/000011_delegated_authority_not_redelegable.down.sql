ALTER TABLE refused_escalations DROP CONSTRAINT IF EXISTS refused_escalations_reason_known;
ALTER TABLE refused_escalations
    ADD CONSTRAINT refused_escalations_reason_known
    CHECK (refusal_reason IN ('self_dealing', 'delegator_mismatch', 'delegator_lacks_authority', 'sod_conflict',
                              'overlap_conflict', 'invalid_window', 'no_create_grant', 'delegate_is_delegator',
                              'delegator_exceeds_limit', 'invalid_limit', 'approval_not_segregated'));
