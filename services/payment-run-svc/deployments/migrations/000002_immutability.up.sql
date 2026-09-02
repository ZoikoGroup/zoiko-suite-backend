-- The runtime connects as Postgres superuser, which bypasses GRANT/REVOKE,
-- so only BEFORE UPDATE/DELETE triggers raising exceptions actually enforce
-- immutability here.

-- payment_runs: DELETE never allowed. COMPLETED/CANCELLED are fully
-- terminal. Once LOCKED, the run's own authorized fields (everything but
-- status/timestamps/idempotency_key/notes) can never change — the literal
-- enforcement of AP-11's own SoD line ("run operator cannot alter
-- authorized fields").
CREATE OR REPLACE FUNCTION reject_run_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment_runs rows are never deleted';
    END IF;

    IF OLD.status IN ('COMPLETED', 'CANCELLED') THEN
        RAISE EXCEPTION 'run % is in terminal status % and cannot be modified', OLD.run_id, OLD.status;
    END IF;

    IF OLD.status NOT IN ('DRAFT', 'VALIDATED') THEN
        IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
            OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
            OR NEW.paying_bank_account_ref IS DISTINCT FROM OLD.paying_bank_account_ref
            OR NEW.currency IS DISTINCT FROM OLD.currency
            OR NEW.value_date IS DISTINCT FROM OLD.value_date
            OR NEW.payment_method IS DISTINCT FROM OLD.payment_method
            OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
            OR NEW.created_at IS DISTINCT FROM OLD.created_at
        THEN
            RAISE EXCEPTION 'run % is locked; its authorized fields can never change', OLD.run_id;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_run_mutation
    BEFORE UPDATE OR DELETE ON payment_runs
    FOR EACH ROW EXECUTE FUNCTION reject_run_mutation();

-- run_instructions: never deleted. The only columns ever permitted to
-- change post-insert are status and consumed_at (both set once, at Lock
-- time, and again only by the reconciliation flow) — everything naming the
-- authorization/payee/amount is fixed at insert.
CREATE OR REPLACE FUNCTION reject_run_instruction_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'run_instructions rows are never deleted';
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.authorization_id IS DISTINCT FROM OLD.authorization_id
        OR NEW.payee_ref IS DISTINCT FROM OLD.payee_ref
        OR NEW.net_amount IS DISTINCT FROM OLD.net_amount
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'run instruction % may only have status/consumed_at change, nothing else', OLD.instruction_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_run_instruction_mutation
    BEFORE UPDATE OR DELETE ON run_instructions
    FOR EACH ROW EXECUTE FUNCTION reject_run_instruction_mutation();

CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rows in % are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_instruction_reconciliation_events_mutation
    BEFORE UPDATE OR DELETE ON instruction_reconciliation_events
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER trg_reject_run_events_mutation
    BEFORE UPDATE OR DELETE ON run_events
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();
