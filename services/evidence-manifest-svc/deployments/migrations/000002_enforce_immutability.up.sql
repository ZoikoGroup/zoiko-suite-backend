-- manifest_records is documented as append-only ("one row per source
-- record pulled into a manifest... so the manifest stays retrievable and
-- reconstructable", 000001_initial_schema.up.sql) but nothing enforced it.
-- A BEFORE trigger enforces it for every connection, including the
-- Postgres superuser this service (like every service in this platform)
-- connects as — GRANT/REVOKE alone cannot, since a superuser bypasses
-- privilege checks entirely.
--
-- evidence_manifests itself is deliberately NOT given this trigger: unlike
-- manifest_records, it has a real mutable lifecycle (status transitions
-- PENDING -> GENERATED/FAILED, backfilling checksum_sha256/failure_reason/
-- generated_at — see PgStore.MarkGenerated/MarkFailed). Only the child
-- records table is append-only.
CREATE OR REPLACE FUNCTION reject_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted on row %',
        TG_TABLE_NAME, TG_OP, OLD.manifest_record_id;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER manifest_records_immutable
    BEFORE UPDATE OR DELETE ON manifest_records
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
