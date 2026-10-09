-- AP-07 -> ACC-04: let an operator requeue a refused or exhausted posting.
--
-- 000006 made every terminal posting status (POSTED, FAILED, QUARANTINED) final.
-- For POSTED that is right: a ledger posting is never repeated. But a FAILED or
-- QUARANTINED request is, by definition, one the ledger did NOT accept (an
-- unresolvable ACC-02 mapping, a locked period, a refused permission, a
-- transient outage that ran out of retries). Leaving those final stranded an
-- approved claim's accounting forever with no way to resubmit once the cause was
-- fixed.
--
-- A FAILED/QUARANTINED request may now move to PENDING -- and to nothing else --
-- which is the operator requeue. The request's basis (payload, source event id,
-- tenant, entity, aggregate) stays immutable, and the ledger is idempotent on
-- source_event_id, so a requeue can never post twice.
CREATE OR REPLACE FUNCTION reject_accounting_posting_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'accounting_posting_requests rows are never deleted';
    END IF;
    IF OLD.status = 'POSTED' THEN
        RAISE EXCEPTION 'accounting posting request % is POSTED and final', OLD.request_id;
    END IF;
    IF OLD.status IN ('FAILED', 'QUARANTINED') AND NEW.status IS DISTINCT FROM 'PENDING' THEN
        RAISE EXCEPTION 'accounting posting request % is % and can only be requeued to PENDING', OLD.request_id, OLD.status;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.aggregate_id IS DISTINCT FROM OLD.aggregate_id
        OR NEW.source_event_id IS DISTINCT FROM OLD.source_event_id
        OR NEW.request_payload IS DISTINCT FROM OLD.request_payload
    THEN
        RAISE EXCEPTION 'accounting posting request % basis can never change', OLD.request_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
