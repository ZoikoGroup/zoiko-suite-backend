-- SPEC_DEVIATIONS A4 (part 2): adds the PENDING_UNKNOWN state the §7 state
-- model names ("PendingUnknown may occur") and invariant 15 requires ("a
-- provider timeout or missing response is Pending/Unknown, never
-- automatically Failed"). Reachable only through the new BNK-06 event
-- consumer (internal/paymentinit) applying BNK-06's own
-- PAYMENT_SUBMISSION_AMBIGUOUS fact — never from a provider callback, which
-- by definition reports a concrete observed status, not an absence of one.
--
-- Three places name the exact set of not-yet-governed/unresolved statuses,
-- and all three must agree or a real state could become either unreachable
-- or silently un-deduplicated:
--   1. The status CHECK constraint (001_initial_schema) - what can be stored.
--   2. uq_payment_execution_states_unresolved_source_reference (007) - which
--      statuses still dedupe a retried source_reference.
--   3. internal/store's unresolvedStatusesSQL constant - the application's own
--      copy of (2)'s predicate, used by RecordPaymentStatus's pre-insert
--      lookup. Keep it in lockstep with this migration (see that file).

ALTER TABLE payment_execution_states
    DROP CONSTRAINT payment_execution_states_status_check;

ALTER TABLE payment_execution_states
    ADD CONSTRAINT payment_execution_states_status_check
    CHECK (status IN ('PREPARED', 'SUBMITTED', 'ACCEPTED', 'PENDING', 'SETTLED',
                       'REJECTED', 'RETURNED', 'CANCELLED', 'PENDING_UNKNOWN'));

-- PENDING_UNKNOWN is unresolved exactly like PREPARED/SUBMITTED/ACCEPTED/
-- PENDING: a retried RecordPaymentStatus for the same source_reference while
-- BNK-06 itself has not yet resolved the ambiguity must still find this row,
-- not create a second one.
DROP INDEX uq_payment_execution_states_unresolved_source_reference;

CREATE UNIQUE INDEX uq_payment_execution_states_unresolved_source_reference
    ON payment_execution_states (COALESCE(tenant_id::text, ''), source_reference)
    WHERE source_reference <> ''
      AND status IN ('PREPARED', 'SUBMITTED', 'ACCEPTED', 'PENDING', 'PENDING_UNKNOWN');
