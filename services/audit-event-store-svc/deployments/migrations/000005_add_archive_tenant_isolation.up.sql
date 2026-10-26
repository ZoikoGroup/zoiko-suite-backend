-- Migration: 000005_add_archive_tenant_isolation.up.sql
--
-- AUD-10 archive tenant isolation.
--
-- Migration 000004 deliberately gave audit_archives / audit_archive_verifications
-- NO tenant_id and NO RLS: at the time the four /v1/archives routes had no
-- tenant context at all, and the header comment argued that because an archive
-- spans a contiguous range of the GLOBAL (cross-tenant) hash chain, "no
-- single-tenant RLS predicate is meaningful here." Access control was left to an
-- authenticated-caller check alone.
--
-- That is a real cross-tenant disclosure gap, not a theoretical one. The
-- caller-supplied X-Tenant-Id is the platform's tenant isolation boundary
-- (Canonical Service Input Contract, INV-01), so an authenticated operator
-- holding AUDIT_ARCHIVE_MANAGE could read ANY tenant's archive metadata and
-- verification history (created_by_principal_id, verification history, the
-- archived sequence ranges) with no tenant predicate anywhere in the path.
-- "Is the range itself global?" is a question about the CONTENT of an archive;
-- it does not answer "which tenant may READ this archive record?".
--
-- This migration makes the archive record itself tenant-owned and tenant-read:
--
--   1. audit_archives.tenant_id / audit_archive_verifications.tenant_id record
--      the tenant on whose behalf the record was created/verified. This is the
--      caller's mandatory tenant context — the same boundary audit_events already
--      uses — not an inferred property of the archived range.
--   2. Both tables get ENABLE + FORCE ROW LEVEL SECURITY with a tenant-only
--      predicate, mirroring 000003's mechanism but deliberately WITHOUT its
--      app.platform_scope escape hatch. Archive records have no global-chain
--      reader analogous to PgStore's chain-tip read, so there is no legitimate
--      reason to bypass tenant scoping here; a platform_scope branch would
--      re-open the exact hole this migration closes.
--   3. Reads (GetArchive, VerifyArchive, ListVerifications) set app.tenant_id for
--      the transaction and additionally carry an explicit tenant predicate in
--      SQL (defense in depth: RLS plus the WHERE clause).
--   4. CreateArchive also sets app.platform_scope — but ONLY so readChainRange
--      can read the global audit_events chain. Because the archive policy below
--      does not reference app.platform_scope, the INSERT's WITH CHECK is still
--      governed by app.tenant_id; setting both together is safe and intended.
--
-- BACKFILL LIMITATION (deliberate, not an oversight): tenant_id is nullable and
-- existing archive/verification rows are NOT backfilled. Nothing in the stored
-- schema records which tenant an already-created archive belonged to — the only
-- actor recorded is created_by_principal_id — so any backfill value would be
-- invented ownership. Under the policy below a NULL tenant_id can never equal
-- app.tenant_id, so pre-existing rows become unreadable through the tenant-scoped
-- API and fail closed rather than leaking to a guessed tenant. If an operator
-- needs a pre-existing archive to remain reachable, that is a data-governance
-- decision (assign an owner) to be taken explicitly, not guessed here.

ALTER TABLE audit_archives
    ADD COLUMN IF NOT EXISTS tenant_id TEXT;

ALTER TABLE audit_archive_verifications
    ADD COLUMN IF NOT EXISTS tenant_id TEXT;

CREATE INDEX IF NOT EXISTS audit_archives_tenant_idx
    ON audit_archives (tenant_id, archive_id);

CREATE INDEX IF NOT EXISTS audit_archive_verifications_tenant_idx
    ON audit_archive_verifications (tenant_id, archive_id, verified_at DESC);

-- FORCE (not just ENABLE) so the policy also applies to the table owner — the
-- same reasoning as 000003. NULLIF guards an app.tenant_id set to the empty
-- string, which must never match a real tenant_id. No platform_scope branch:
-- see the header.
ALTER TABLE audit_archives ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_archives FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS archive_tenant_isolation_policy ON audit_archives;
CREATE POLICY archive_tenant_isolation_policy ON audit_archives
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE audit_archive_verifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_archive_verifications FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS archive_verification_tenant_isolation_policy ON audit_archive_verifications;
CREATE POLICY archive_verification_tenant_isolation_policy ON audit_archive_verifications
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
