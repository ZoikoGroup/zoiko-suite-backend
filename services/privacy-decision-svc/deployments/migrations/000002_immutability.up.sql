-- 000002_immutability.up.sql
-- privacy_decisions is a pure append-only evidence log — §13.2 "decision
-- durability" requires historical reproduction to never depend on later
-- state, which a mutable row would defeat outright. Same doctrine and
-- same generic trigger function shape as every other evidence table
-- built this session (privacy-consent-svc's four evidence tables).
CREATE OR REPLACE FUNCTION reject_decision_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'privacy_decisions is append-only evidence: % is never permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER privacy_decisions_append_only
    BEFORE UPDATE OR DELETE ON privacy_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_decision_mutation();
