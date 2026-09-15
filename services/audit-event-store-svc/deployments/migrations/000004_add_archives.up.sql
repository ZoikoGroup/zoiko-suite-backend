-- Migration: 000004_add_archives.up.sql
--
-- AUD-10 (Audit Trail / Data Export), archive half. Implements CreateArchive
-- and VerifyArchive directly on top of this service's own hash chain
-- (migration 000002/000003), with zero cross-service knowledge — the
-- approve/redact/deliver half of AUD-10 lives entirely in
-- reporting-orchestration-svc, deliberately kept out of this append-only
-- ledger service.
--
-- audit_archives is itself append-only, same as audit_events: an archive is
-- a permanent record of "as of this creation, sequences [from,to] formed an
-- unbroken chain with this digest." Nothing about that claim is ever
-- editable after the fact — a later divergence is recorded as a NEW row in
-- audit_archive_verifications, never as a mutation of the archive itself.
--
-- Neither table carries tenant_id: an archive spans a contiguous
-- sequence-number range of the GLOBAL chain (see migration 000003's own
-- rationale for why the chain itself is cross-tenant by design), so no
-- single-tenant RLS predicate is meaningful here. Access control for these
-- two new HTTP routes is therefore an authenticated-caller check
-- (X-Principal-Id, enforced by the envelope middleware already wired into
-- this service) rather than tenant-scoped RLS.

CREATE TABLE IF NOT EXISTS audit_archives (
    archive_id              TEXT        NOT NULL,
    from_sequence           BIGINT      NOT NULL,
    to_sequence             BIGINT      NOT NULL,
    event_count             BIGINT      NOT NULL,
    archive_digest          TEXT        NOT NULL,
    first_event_hash        TEXT        NOT NULL,
    last_event_hash         TEXT        NOT NULL,
    created_by_principal_id TEXT        NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT audit_archives_pkey PRIMARY KEY (archive_id),
    CONSTRAINT audit_archives_range_valid CHECK (to_sequence >= from_sequence),
    CONSTRAINT audit_archives_count_positive CHECK (event_count > 0)
);

CREATE INDEX IF NOT EXISTS audit_archives_range_idx
    ON audit_archives (from_sequence, to_sequence);

-- audit_archives is fully append-only — no UPDATE, no DELETE, ever. This is
-- the DB-enforced half of "an archive's claim about a chain range is
-- permanent"; VerifyArchive re-derives today's truth independently rather
-- than editing yesterday's claim.
CREATE OR REPLACE FUNCTION reject_archive_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_archives is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_archive_update
    BEFORE UPDATE ON audit_archives
    FOR EACH ROW EXECUTE FUNCTION reject_archive_mutation();
CREATE TRIGGER trg_reject_archive_delete
    BEFORE DELETE ON audit_archives
    FOR EACH ROW EXECUTE FUNCTION reject_archive_mutation();

CREATE TABLE IF NOT EXISTS audit_archive_verifications (
    verification_id          TEXT        NOT NULL,
    archive_id                TEXT        NOT NULL REFERENCES audit_archives(archive_id),
    verified_by_principal_id  TEXT        NOT NULL,
    verified_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    result                     TEXT        NOT NULL CHECK (result IN ('VERIFIED', 'DIVERGED')),
    first_divergent_sequence  BIGINT,

    CONSTRAINT audit_archive_verifications_pkey PRIMARY KEY (verification_id),
    CONSTRAINT audit_archive_verifications_divergence_shape
        CHECK (result = 'VERIFIED' OR first_divergent_sequence IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS audit_archive_verifications_archive_idx
    ON audit_archive_verifications (archive_id, verified_at DESC);

-- Verification history is append-only too: every VerifyArchive call, VERIFIED
-- or DIVERGED, is itself a permanent audit fact and is never edited or
-- retried-in-place.
CREATE OR REPLACE FUNCTION reject_archive_verification_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_archive_verifications is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_archive_verification_update
    BEFORE UPDATE ON audit_archive_verifications
    FOR EACH ROW EXECUTE FUNCTION reject_archive_verification_mutation();
CREATE TRIGGER trg_reject_archive_verification_delete
    BEFORE DELETE ON audit_archive_verifications
    FOR EACH ROW EXECUTE FUNCTION reject_archive_verification_mutation();
