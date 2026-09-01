-- The runtime connects as Postgres superuser, which bypasses GRANT/REVOKE,
-- so only BEFORE UPDATE/DELETE triggers raising exceptions actually enforce
-- immutability here.

-- payable_open_items: never deleted. Once closed (closed_at set), fully
-- terminal — no further change of any kind. While open, the fields that
-- identify and size the original liability (legal entity, source, payee,
-- original amount, currency, due date, creator/creation time) can never
-- change — the literal enforcement of the spec's own SoD line ("no
-- free-form balance edit"). residual_amount/status/hold/dispute/closed_at/
-- updated_at change only through this service's own specific command
-- handlers, never a raw field-level update.
CREATE OR REPLACE FUNCTION reject_payable_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payable_open_items rows are never deleted';
    END IF;

    IF OLD.closed_at IS NOT NULL THEN
        RAISE EXCEPTION 'payable % is closed and cannot be modified', OLD.payable_id;
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.source_type IS DISTINCT FROM OLD.source_type
        OR NEW.source_reference IS DISTINCT FROM OLD.source_reference
        OR NEW.payee_ref IS DISTINCT FROM OLD.payee_ref
        OR NEW.original_amount IS DISTINCT FROM OLD.original_amount
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.due_date IS DISTINCT FROM OLD.due_date
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'payable % authorized fields can never change once created', OLD.payable_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_payable_mutation
    BEFORE UPDATE OR DELETE ON payable_open_items
    FOR EACH ROW EXECUTE FUNCTION reject_payable_mutation();

CREATE OR REPLACE FUNCTION reject_settlement_application_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'settlement_applications rows are append-only and can never be updated or deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_settlement_application_mutation
    BEFORE UPDATE OR DELETE ON settlement_applications
    FOR EACH ROW EXECUTE FUNCTION reject_settlement_application_mutation();
