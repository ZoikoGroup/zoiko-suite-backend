-- The runtime connects as Postgres superuser, which bypasses GRANT/REVOKE,
-- so only BEFORE UPDATE/DELETE triggers raising exceptions actually enforce
-- immutability here.

-- payment_initiation_attempts: DELETE never allowed. SUBMITTED is terminal
-- from THIS service's own perspective — the spec's own words: "external
-- execution/finality belongs to BNK-07," not BNK-06. REJECTED_BEFORE_
-- SUBMISSION/CANCELLED/QUARANTINED are all terminal too. Once PREPARED (the
-- very first row ever written for an attempt), the authorized fields can
-- never change — the literal fix for negative-path "payment amount changed
-- after authorization."
CREATE OR REPLACE FUNCTION reject_attempt_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment_initiation_attempts rows are never deleted';
    END IF;

    IF OLD.status IN ('SUBMITTED', 'REJECTED_BEFORE_SUBMISSION', 'CANCELLED', 'QUARANTINED') THEN
        RAISE EXCEPTION 'attempt % is in terminal status % and cannot be modified', OLD.attempt_id, OLD.status;
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.source_reference IS DISTINCT FROM OLD.source_reference
        OR NEW.authorization_fingerprint IS DISTINCT FROM OLD.authorization_fingerprint
        OR NEW.payer_account_ref IS DISTINCT FROM OLD.payer_account_ref
        OR NEW.payee_ref IS DISTINCT FROM OLD.payee_ref
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.execution_date IS DISTINCT FROM OLD.execution_date
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'attempt % is prepared; its authorized fields can never change', OLD.attempt_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_attempt_mutation
    BEFORE UPDATE OR DELETE ON payment_initiation_attempts
    FOR EACH ROW EXECUTE FUNCTION reject_attempt_mutation();

CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rows in % are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_attempt_events_mutation
    BEFORE UPDATE OR DELETE ON attempt_events
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();
