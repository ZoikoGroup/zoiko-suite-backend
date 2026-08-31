-- The runtime connects as Postgres superuser, which bypasses GRANT/REVOKE,
-- so only BEFORE UPDATE/DELETE triggers raising exceptions actually enforce
-- immutability here.

-- payment_execution_states: DELETE never allowed. Its own identity fields
-- (tenant/legal entity/provider_request_id/source_reference/creator) never
-- change. Once REJECTED or CANCELLED, fully terminal. Once SETTLED, the
-- ONLY further transition ever permitted is to RETURNED (the explicit,
-- distinct return/reversal semantics the spec names) — everything else
-- about a settled payment is frozen. RETURNED is itself then fully
-- terminal. This is the literal enforcement of negative-path scenario #2
-- ("out-of-order status regresses settled payment").
CREATE OR REPLACE FUNCTION reject_execution_state_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment_execution_states rows are never deleted';
    END IF;

    IF OLD.status IN ('REJECTED', 'CANCELLED', 'RETURNED') THEN
        RAISE EXCEPTION 'payment % is in terminal status % and cannot be modified', OLD.payment_id, OLD.status
            USING ERRCODE = 'ZK002';
    END IF;

    IF OLD.status = 'SETTLED' AND NEW.status <> 'SETTLED' AND NEW.status <> 'RETURNED' THEN
        RAISE EXCEPTION 'payment % is settled; the only further transition allowed is RETURNED', OLD.payment_id
            USING ERRCODE = 'ZK002';
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.provider_request_id IS DISTINCT FROM OLD.provider_request_id
        OR NEW.source_reference IS DISTINCT FROM OLD.source_reference
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'payment % identity fields can never change', OLD.payment_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_execution_state_mutation
    BEFORE UPDATE OR DELETE ON payment_execution_states
    FOR EACH ROW EXECUTE FUNCTION reject_execution_state_mutation();

CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rows in % are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_status_events_mutation
    BEFORE UPDATE OR DELETE ON status_events
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();
