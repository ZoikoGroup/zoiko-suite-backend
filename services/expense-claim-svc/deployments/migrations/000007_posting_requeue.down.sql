-- Restore 000006's trigger: POSTED, FAILED and QUARANTINED are all final.
CREATE OR REPLACE FUNCTION reject_accounting_posting_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'accounting_posting_requests rows are never deleted';
    END IF;
    IF OLD.status IN ('POSTED', 'FAILED', 'QUARANTINED') THEN
        RAISE EXCEPTION 'accounting posting request % is final (%)', OLD.request_id, OLD.status;
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
