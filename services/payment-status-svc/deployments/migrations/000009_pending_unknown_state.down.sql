-- Refused while any row carries PENDING_UNKNOWN: narrowing the CHECK back
-- would make the ALTER TABLE itself fail validation against those rows
-- anyway, but a named, legible refusal here is better than that opaque
-- constraint-violation error, and it fails before the index is touched.
--
-- payment_execution_states is FORCE ROW LEVEL SECURITY (000003), so a role
-- subject to row security would otherwise see only its current app.tenant_id
-- scope and could pass on an empty view while another tenant's row is still
-- PENDING_UNKNOWN. row_security is turned off first: a role that may bypass
-- row security then sees every tenant's rows, and one that may not fails the
-- query instead — a refusal, never a silent loss.
SET LOCAL row_security = off;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM payment_execution_states WHERE status = 'PENDING_UNKNOWN') THEN
        RAISE EXCEPTION 'cannot roll back 000009: payments are in PENDING_UNKNOWN';
    END IF;
END $$;

DROP INDEX uq_payment_execution_states_unresolved_source_reference;

CREATE UNIQUE INDEX uq_payment_execution_states_unresolved_source_reference
    ON payment_execution_states (COALESCE(tenant_id::text, ''), source_reference)
    WHERE source_reference <> ''
      AND status IN ('PREPARED', 'SUBMITTED', 'ACCEPTED', 'PENDING');

ALTER TABLE payment_execution_states
    DROP CONSTRAINT payment_execution_states_status_check;

ALTER TABLE payment_execution_states
    ADD CONSTRAINT payment_execution_states_status_check
    CHECK (status IN ('PREPARED', 'SUBMITTED', 'ACCEPTED', 'PENDING', 'SETTLED',
                       'REJECTED', 'RETURNED', 'CANCELLED'));
