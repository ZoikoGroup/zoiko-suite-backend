-- The runtime connects as Postgres superuser, which bypasses GRANT/REVOKE,
-- so only BEFORE UPDATE/DELETE triggers raising exceptions enforce
-- immutability here.

-- supplier_recovery_cases: never deleted. Once CLOSED or WRITTEN_OFF,
-- fully terminal. While active, the fields that identify and size the
-- original recovery (legal entity, supplier, basis, source payable,
-- total amount, currency, creator/creation time) can never change —
-- recovered_amount/status/escalation/write-off/close fields/updated_at
-- change only through this service's own specific command handlers.
CREATE OR REPLACE FUNCTION reject_recovery_case_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'supplier_recovery_cases rows are never deleted';
    END IF;

    IF OLD.status IN ('CLOSED', 'WRITTEN_OFF') THEN
        RAISE EXCEPTION 'recovery case % is terminal (%), and cannot be modified', OLD.case_id, OLD.status;
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.supplier_ref IS DISTINCT FROM OLD.supplier_ref
        OR NEW.recovery_basis IS DISTINCT FROM OLD.recovery_basis
        OR NEW.source_payable_id IS DISTINCT FROM OLD.source_payable_id
        OR NEW.total_amount IS DISTINCT FROM OLD.total_amount
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'recovery case % authorized fields can never change once created', OLD.case_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_recovery_case_mutation
    BEFORE UPDATE OR DELETE ON supplier_recovery_cases
    FOR EACH ROW EXECUTE FUNCTION reject_recovery_case_mutation();

CREATE OR REPLACE FUNCTION reject_recovery_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rows in % are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_recovery_applications_mutation
    BEFORE UPDATE OR DELETE ON recovery_applications
    FOR EACH ROW EXECUTE FUNCTION reject_recovery_evidence_mutation();

CREATE TRIGGER trg_reject_recovery_commitments_mutation
    BEFORE UPDATE OR DELETE ON recovery_commitments
    FOR EACH ROW EXECUTE FUNCTION reject_recovery_evidence_mutation();
