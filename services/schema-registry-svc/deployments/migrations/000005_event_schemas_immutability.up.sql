-- 000005_event_schemas_immutability.up.sql
--
-- A registered schema version is immutable and append-only (openapi.yaml
-- "Invariants": "Versions are immutable and append-only. There is no update and
-- no delete"; Data Model §2.12 "version-controlled schema definitions"). Until
-- now that held only because the API has no update or delete route. The primary
-- key (event_name, version) does NOT make a row immutable — it only refuses a
-- second row with the same key — so any writer with UPDATE or DELETE on the
-- table could rewrite or remove a registered contract.
--
-- These triggers reject UPDATE, DELETE and TRUNCATE for every role, the table
-- owner and superusers included. INSERT — the only statement the service issues
-- against this table — is untouched, so registration, replay and the
-- optimistic-concurrency insert behave exactly as before. No privilege is
-- granted or revoked.
--
-- Honest limit: a superuser/owner can still DISABLE the triggers
-- (ALTER TABLE ... DISABLE TRIGGER, or session_replication_role = replica). That
-- is an administrative act the application cannot perform.

CREATE OR REPLACE FUNCTION reject_event_schema_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'event_schemas is append-only: % is not permitted', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_event_schemas_reject_update ON event_schemas;
CREATE TRIGGER trg_event_schemas_reject_update
    BEFORE UPDATE ON event_schemas
    FOR EACH ROW EXECUTE FUNCTION reject_event_schema_mutation();

DROP TRIGGER IF EXISTS trg_event_schemas_reject_delete ON event_schemas;
CREATE TRIGGER trg_event_schemas_reject_delete
    BEFORE DELETE ON event_schemas
    FOR EACH ROW EXECUTE FUNCTION reject_event_schema_mutation();

DROP TRIGGER IF EXISTS trg_event_schemas_reject_truncate ON event_schemas;
CREATE TRIGGER trg_event_schemas_reject_truncate
    BEFORE TRUNCATE ON event_schemas
    FOR EACH STATEMENT EXECUTE FUNCTION reject_event_schema_mutation();
