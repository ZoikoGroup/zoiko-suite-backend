-- The runtime connects as Postgres superuser, which bypasses GRANT/REVOKE,
-- so only BEFORE UPDATE/DELETE triggers raising exceptions enforce
-- immutability here.

-- payee_destinations: never deleted. Once SUSPENDED or SUPERSEDED, fully
-- terminal — the spec's own command list has no reactivation path, so a
-- changed beneficiary always means proposing a brand new destination, never
-- resurrecting an old one. While active, the fields that identify the real
-- destination (legal entity, party, scope, institution, account,
-- currency, country, payee name, source, fingerprint, proposer/creation
-- time) can never change — status/verification/approval/supersede/suspend
-- fields change only through this service's own specific command
-- handlers.
CREATE OR REPLACE FUNCTION reject_payee_destination_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payee_destinations rows are never deleted';
    END IF;

    IF OLD.status IN ('SUSPENDED', 'SUPERSEDED') THEN
        RAISE EXCEPTION 'destination % is terminal (%), and cannot be modified', OLD.destination_id, OLD.status;
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.party_ref IS DISTINCT FROM OLD.party_ref
        OR NEW.scope IS DISTINCT FROM OLD.scope
        OR NEW.financial_institution IS DISTINCT FROM OLD.financial_institution
        OR NEW.account_identifier IS DISTINCT FROM OLD.account_identifier
        OR NEW.account_last4 IS DISTINCT FROM OLD.account_last4
        OR NEW.country_code IS DISTINCT FROM OLD.country_code
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.payee_name IS DISTINCT FROM OLD.payee_name
        OR NEW.source_type IS DISTINCT FROM OLD.source_type
        OR NEW.fingerprint IS DISTINCT FROM OLD.fingerprint
        OR NEW.proposed_by_principal_id IS DISTINCT FROM OLD.proposed_by_principal_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'destination % authorized fields can never change once proposed', OLD.destination_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_payee_destination_mutation
    BEFORE UPDATE OR DELETE ON payee_destinations
    FOR EACH ROW EXECUTE FUNCTION reject_payee_destination_mutation();

CREATE OR REPLACE FUNCTION reject_payee_event_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rows in % are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_payee_destination_events_mutation
    BEFORE UPDATE OR DELETE ON payee_destination_events
    FOR EACH ROW EXECUTE FUNCTION reject_payee_event_mutation();
