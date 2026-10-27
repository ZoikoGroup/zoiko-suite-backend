-- 000013_document_versions_and_access_log_immutability.up.sql
--
-- document_versions and document_access_log are append-only evidence:
--   * ZS-SVC-S-001 DRC-I01 "A committed document_version_id always resolves to
--     the same bytes and content hash" and DRC-I02 "Existing version bytes are
--     never overwritten; correction creates a new version" (§3.3);
--   * the access log is the record of who read what (§10.1, DRC-I26).
--
-- Migration 000001 declared both tables append-only in its header, and the
-- service's audit file said so, but nothing enforced it: the API has no route
-- that updates or deletes either table, which is a property of the handlers,
-- not of the database. Any writer with UPDATE/DELETE on the tables — the
-- application's own role included — could change a committed version's
-- checksum, storage key or size, or rewrite or erase the access log.
--
-- These triggers reject UPDATE, DELETE and TRUNCATE on both tables for every
-- role, the table owner and superusers included. INSERT is untouched (new
-- versions and new log entries are appended exactly as before), and so are the
-- row-level-security policies from 000002. documents itself is deliberately NOT
-- covered: its current_version pointer and lifecycle columns legitimately
-- change, and 000005/000006 already guard the parts that must not.
--
-- Honest limit: a superuser or table owner can still DISABLE the triggers
-- (ALTER TABLE ... DISABLE TRIGGER, or session_replication_role = replica).
-- That is an administrative act the application cannot perform; the stored
-- SHA-256 per version, recomputed on every read and by verify-digest, is what
-- exposes a tampered blob. Nothing in this service deletes versions today;
-- when a disposition path that really destroys content is built, it will need
-- an explicit, audited exception here — silently relaxing these triggers is
-- not the way to do it.

CREATE OR REPLACE FUNCTION reject_append_only_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only evidence: % is not permitted', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_document_versions_reject_update ON document_versions;
CREATE TRIGGER trg_document_versions_reject_update
    BEFORE UPDATE ON document_versions
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();
DROP TRIGGER IF EXISTS trg_document_versions_reject_delete ON document_versions;
CREATE TRIGGER trg_document_versions_reject_delete
    BEFORE DELETE ON document_versions
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();
DROP TRIGGER IF EXISTS trg_document_versions_reject_truncate ON document_versions;
CREATE TRIGGER trg_document_versions_reject_truncate
    BEFORE TRUNCATE ON document_versions
    FOR EACH STATEMENT EXECUTE FUNCTION reject_append_only_mutation();

DROP TRIGGER IF EXISTS trg_document_access_log_reject_update ON document_access_log;
CREATE TRIGGER trg_document_access_log_reject_update
    BEFORE UPDATE ON document_access_log
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();
DROP TRIGGER IF EXISTS trg_document_access_log_reject_delete ON document_access_log;
CREATE TRIGGER trg_document_access_log_reject_delete
    BEFORE DELETE ON document_access_log
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();
DROP TRIGGER IF EXISTS trg_document_access_log_reject_truncate ON document_access_log;
CREATE TRIGGER trg_document_access_log_reject_truncate
    BEFORE TRUNCATE ON document_access_log
    FOR EACH STATEMENT EXECUTE FUNCTION reject_append_only_mutation();
