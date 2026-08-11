-- workflow_history_events is documented as append-only ("No UPDATE or
-- DELETE permitted. This table is append-only evidence.",
-- 000001_initial_schema.up.sql) but nothing enforced it beyond the
-- application never issuing one. A BEFORE trigger enforces it for every
-- connection, including the Postgres superuser this service (like every
-- service in this platform) connects as — GRANT/REVOKE alone cannot,
-- since a superuser bypasses privilege checks entirely.
CREATE OR REPLACE FUNCTION reject_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted on row %',
        TG_TABLE_NAME, TG_OP, OLD.event_id;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workflow_history_events_immutable
    BEFORE UPDATE OR DELETE ON workflow_history_events
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
