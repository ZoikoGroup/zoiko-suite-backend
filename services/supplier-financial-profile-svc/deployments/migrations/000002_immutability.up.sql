-- 000002_immutability.up.sql
-- Enforces immutability at the database layer — GRANT/REVOKE cannot bind
-- the superuser this runtime connects as, so only a trigger actually
-- stops it (same reasoning as every other immutability migration in this
-- platform).

-- supplier_financial_profiles cycles legitimately (ACTIVE <-> ON_HOLD,
-- ACTIVE <-> SUSPENDED) until RETIRED, which is terminal — same doctrine
-- as privacy-rights-svc's CLOSED rights_requests.
CREATE OR REPLACE FUNCTION reject_retired_profile_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'supplier_financial_profiles row % is RETIRED and immutable', OLD.profile_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER supplier_financial_profiles_retired_immutable
    BEFORE UPDATE ON supplier_financial_profiles
    FOR EACH ROW EXECUTE FUNCTION reject_retired_profile_mutation();

-- payment_terms_periods and profile_change_events are pure append-only
-- evidence — every row, once written, is permanent.
CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is never permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER payment_terms_periods_append_only
    BEFORE UPDATE OR DELETE ON payment_terms_periods
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER profile_change_events_append_only
    BEFORE UPDATE OR DELETE ON profile_change_events
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

-- high_risk_change_requests gets exactly one legitimate transition —
-- PENDING_APPROVAL -> APPROVED/REJECTED, recording who decided it and
-- when. Once decided, the row is as permanent as any evidence table;
-- before that, only the decision columns may ever change.
CREATE OR REPLACE FUNCTION reject_decided_change_request_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status <> 'PENDING_APPROVAL' THEN
        RAISE EXCEPTION 'high_risk_change_requests row % is already decided (%) and immutable', OLD.change_request_id, OLD.status;
    END IF;
    IF NEW.profile_id IS DISTINCT FROM OLD.profile_id
       OR NEW.field IS DISTINCT FROM OLD.field
       OR NEW.old_value IS DISTINCT FROM OLD.old_value
       OR NEW.new_value IS DISTINCT FROM OLD.new_value
       OR NEW.proposed_by_principal_id IS DISTINCT FROM OLD.proposed_by_principal_id
       OR NEW.proposed_at IS DISTINCT FROM OLD.proposed_at
    THEN
        RAISE EXCEPTION 'high_risk_change_requests proposal fields are immutable (row %)', OLD.change_request_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER high_risk_change_requests_decision_only
    BEFORE UPDATE ON high_risk_change_requests
    FOR EACH ROW EXECUTE FUNCTION reject_decided_change_request_mutation();
