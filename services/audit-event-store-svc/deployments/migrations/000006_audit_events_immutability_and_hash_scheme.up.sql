-- Migration: 000006_audit_events_immutability_and_hash_scheme.up.sql
--
-- Two service-owned integrity requirements for audit_events
-- (Microservices Specification §14.1 "Records must be immutable";
-- Data Model §15.4 "tamper-evident integrity chain").
--
-- 1. DATABASE-LEVEL IMMUTABILITY
--
--    Until now audit_events was append-only only by application convention:
--    nothing in the database stopped an UPDATE or DELETE (the archive tables
--    had triggers, the event table itself did not), so a privileged writer —
--    including the platform-scope ingestion path, whose RLS policy is FOR ALL —
--    could rewrite evidence. These triggers reject UPDATE, DELETE and TRUNCATE
--    for every role, the table owner and superusers included. INSERT, which is
--    the only thing the ingestion path does, is untouched; RLS and FORCE RLS
--    are unchanged.
--
--    Honest limit: a superuser/owner can still DISABLE the triggers
--    (ALTER TABLE ... DISABLE TRIGGER, or session_replication_role = replica).
--    That is an administrative act, not something the application can do, and
--    the hash chain (below) exists precisely so such an act is detectable.
--
-- 2. PAYLOAD HASH SCHEME
--
--    payload_hash was SHA-256 over the raw bytes the producer sent, but the
--    payload column is JSONB, which does not keep those bytes (whitespace and
--    key order are normalised). A stored payload therefore could not be
--    re-hashed to check it, and the chain verification only compared stored
--    hashes with each other. Rows written from now on hash the canonical
--    JSONB text of the payload (sha256 over payload::text), which the database
--    can recompute from the stored column at any time. payload_hash_scheme
--    records which rule produced a row's hash:
--
--      NULL                    legacy row: hash over the producer's raw bytes.
--                              Its payload cannot be re-derived from the stored
--                              JSONB, so verification checks its sequence and
--                              link only. It is NOT rewritten (audit_events is
--                              immutable) and no scheme is guessed for it.
--      'sha256-jsonb-text-v1'  hash = lower-hex SHA-256 of payload::text.
--
--    The hash still covers the payload only (and the link to the previous
--    row's payload_hash); it does not cover tenant, type or other columns.

ALTER TABLE audit_events
    ADD COLUMN IF NOT EXISTS payload_hash_scheme TEXT;

CREATE OR REPLACE FUNCTION reject_audit_event_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only evidence: % is not permitted', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_audit_events_reject_update ON audit_events;
CREATE TRIGGER trg_audit_events_reject_update
    BEFORE UPDATE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION reject_audit_event_mutation();

DROP TRIGGER IF EXISTS trg_audit_events_reject_delete ON audit_events;
CREATE TRIGGER trg_audit_events_reject_delete
    BEFORE DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION reject_audit_event_mutation();

DROP TRIGGER IF EXISTS trg_audit_events_reject_truncate ON audit_events;
CREATE TRIGGER trg_audit_events_reject_truncate
    BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION reject_audit_event_mutation();
