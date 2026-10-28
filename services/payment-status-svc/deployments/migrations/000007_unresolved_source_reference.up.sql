-- ZS-SVC-M-001 BNK-07 A5, enforced by the database rather than only by a
-- check that two concurrent RecordPaymentStatus calls can both pass.
--
-- RecordPaymentStatus is a projection of a BNK-06 attempt. A lost 201 followed
-- by a retry must return the execution state that already exists for that
-- attempt, not create a second one that can receive its own callbacks and emit
-- its own PaymentSettled - the duplicate-accounting effect negative-path #3
-- prevents.
--
-- The natural key is (tenant_id, source_reference): the same concept and field
-- name BNK-06's own idempotent-dedup fix uses for its unresolved attempts
-- (payment-initiation-adapter-svc migration
-- 000007_unresolved_source_reference). provider_request_id is deliberately not
-- the key: it is optional and allowed to be empty, so it cannot identify a
-- retry.
--
-- The indexed statuses are this service's not-yet-resolved states: PREPARED,
-- SUBMITTED, ACCEPTED and PENDING (the complement of domain.finalStates).
-- SETTLED, REJECTED, CANCELLED and RETURNED are resolved, so a genuine later
-- instruction may reuse the source_reference. An empty source_reference is
-- excluded: there is nothing to dedupe against.
--
-- tenant_id is nullable on legacy/tenant-less rows; COALESCE gives those rows
-- one empty tenant key so two NULL-tenant rows still collide, matching the
-- application's lookup predicate.
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_execution_states_unresolved_source_reference
    ON payment_execution_states (COALESCE(tenant_id::text, ''), source_reference)
    WHERE source_reference <> ''
      AND status IN ('PREPARED', 'SUBMITTED', 'ACCEPTED', 'PENDING');
