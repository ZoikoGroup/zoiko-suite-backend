-- 000002_immutability.up.sql
-- Enforces immutability at the database layer — GRANT/REVOKE cannot bind
-- the superuser this runtime connects as, so only a trigger actually
-- stops it (same reasoning as every other immutability migration in this
-- platform).
--
-- rights_requests is the one legitimately mutable row in this schema
-- while OPEN (status/identity_verified/outcome progress as the case
-- moves) — but once CLOSED, nothing about it should ever change again.
CREATE OR REPLACE FUNCTION reject_closed_request_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status = 'CLOSED' THEN
        RAISE EXCEPTION 'rights_requests row % is CLOSED and immutable', OLD.request_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER rights_requests_closed_immutable
    BEFORE UPDATE ON rights_requests
    FOR EACH ROW EXECUTE FUNCTION reject_closed_request_mutation();

-- identity_verification_events and discovery_manifests are pure
-- append-only evidence logs — every row, once written, is permanent.
CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only evidence: % is never permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER identity_verification_events_append_only
    BEFORE UPDATE OR DELETE ON identity_verification_events
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER discovery_manifests_append_only
    BEFORE UPDATE OR DELETE ON discovery_manifests
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();
