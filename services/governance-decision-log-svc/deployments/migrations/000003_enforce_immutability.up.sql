-- governance_decisions has been documented as append-only since
-- 000001_initial_schema.up.sql's package doc comment ("No UPDATE or DELETE
-- on any stored decision — ever"), but nothing actually enforced it: any
-- connection with table access — including the Postgres superuser every
-- service in this platform connects as — could UPDATE or DELETE a row
-- undetected. GRANT/REVOKE cannot close this gap on its own, since a
-- superuser bypasses privilege checks entirely; a BEFORE trigger does not
-- bypass for superusers, so this is the actual enforcement mechanism.
CREATE OR REPLACE FUNCTION reject_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted on row %',
        TG_TABLE_NAME, TG_OP, OLD.decision_id;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER governance_decisions_immutable
    BEFORE UPDATE OR DELETE ON governance_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
