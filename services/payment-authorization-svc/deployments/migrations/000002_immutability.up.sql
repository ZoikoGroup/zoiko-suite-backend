-- The runtime connects as Postgres superuser, which bypasses GRANT/REVOKE,
-- so only BEFORE UPDATE/DELETE triggers raising exceptions actually enforce
-- immutability here.

-- payment_authorizations: DELETE never allowed. REJECTED/CONSUMED/REVOKED/
-- EXPIRED/INVALIDATED are all terminal — this is the literal enforcement of
-- negative-path scenario #4 (a CONSUMED authorization can never be
-- replayed: ConsumePaymentAuthorization's own WHERE status = 'APPROVED'
-- guard already prevents a second consume from matching a row, and this
-- trigger is the second, database-level layer making that permanent).
CREATE OR REPLACE FUNCTION reject_authorization_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment_authorizations rows are never deleted';
    END IF;

    IF OLD.status IN ('REJECTED', 'CONSUMED', 'REVOKED', 'EXPIRED', 'INVALIDATED') THEN
        RAISE EXCEPTION 'authorization % is in terminal status % and cannot be modified', OLD.authorization_id, OLD.status;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_authorization_mutation
    BEFORE UPDATE OR DELETE ON payment_authorizations
    FOR EACH ROW EXECUTE FUNCTION reject_authorization_mutation();

CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rows in % are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_authorization_payee_snapshots_mutation
    BEFORE UPDATE OR DELETE ON authorization_payee_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER trg_reject_authorization_events_mutation
    BEFORE UPDATE OR DELETE ON authorization_events
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();
