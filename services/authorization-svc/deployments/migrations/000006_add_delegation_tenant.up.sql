-- Migration: 000006_add_delegation_tenant.up.sql
--
-- Gives delegated_authorities the tenant_id column it never had, and the row
-- security that column makes possible. It is the last table in this schema
-- with no row-level security at all.
--
-- ── Why 000004 skipped it, and why that no longer holds ──────────────────────
--
-- 000004 wrote: "Fabricating a tenant_id column on tables that were never
-- given one is a data-model change, not an RLS migration". That was right for
-- an RLS pass. This migration IS the data-model change, done deliberately and
-- with the service INSERT updated in the same commit — which is the part that
-- was missing before, and the reason the Supabase 0028 pass also left this
-- table alone.
--
-- ── What was exposed ─────────────────────────────────────────────────────────
--
-- A delegation is "principal X may act with principal Y's authority". With no
-- tenant column there was no path to a tenant to write a policy against, so
-- every read and write of this table ran unscoped:
--
--   CreateDelegatedAuthority     INSERT with no tenant, on the bare pool
--   FindDelegatedAuthorityByID   WHERE delegated_authority_id = $1 only
--   RevokeDelegatedAuthority     WHERE delegated_authority_id = $1 only
--   FindDelegatedActions         WHERE delegate_principal_id = $1 only
--
-- The handler's own ownership check (delegator must be the caller) stopped a
-- caller inventing a delegation FROM someone else. Nothing stopped one tenant's
-- delegation being read, revoked, or evaluated under another tenant's scope.
--
-- ── NOT NULL, and why this migration would rather fail than proceed ──────────
--
-- The column is NOT NULL because a NULL tenant here is not "global" — it is a
-- row that no policy can ever match, so the delegation exists and silently
-- never grants anything. That is the worst of the available outcomes: an
-- authority that appears configured and is inert.
--
-- The table held 0 rows when this was surveyed, so the backfill is empty. If
-- that has changed, this migration stops and says so rather than inventing a
-- tenant for a row describing who may act for whom.

ALTER TABLE delegated_authorities ADD COLUMN IF NOT EXISTS tenant_id UUID;

DO $$
DECLARE untenanted int;
BEGIN
    SELECT count(*) INTO untenanted FROM delegated_authorities WHERE tenant_id IS NULL;
    IF untenanted > 0 THEN
        RAISE EXCEPTION
            '% delegated_authorities rows have no tenant_id. Backfill them from the delegator''s tenant before re-running: a NULL tenant matches no policy, so those delegations would exist and never grant anything.',
            untenanted;
    END IF;
END
$$;

ALTER TABLE delegated_authorities ALTER COLUMN tenant_id SET NOT NULL;

-- The evaluation lookup FindDelegatedActions performs, now tenant-first.
-- idx_delegations_lookup (000001) stays: it still serves the revocation-status
-- filter, and dropping an index a live query plan may be using is not this
-- migration's business.
CREATE INDEX IF NOT EXISTS idx_delegations_tenant_lookup
    ON delegated_authorities (tenant_id, delegate_principal_id, revocation_status);

ALTER TABLE delegated_authorities ENABLE ROW LEVEL SECURITY;
ALTER TABLE delegated_authorities FORCE  ROW LEVEL SECURITY;

-- Dropped first so this file is re-runnable. authorization_svc reaches a
-- Supabase project through deployments/supabase applying these migrations AND
-- through supabase/migrations/0031 being pasted; whichever lands first, the
-- other must be a no-op rather than an error.
--
-- No platform_scope hatch, unlike roles. Nothing needs to discover which tenant
-- owns an unknown delegation_id: every caller already knows its own tenant, and
-- FindDelegatedActions is reached from /v1/authorize which resolves one.
DROP POLICY IF EXISTS tenant_isolation_policy ON delegated_authorities;
CREATE POLICY tenant_isolation_policy ON delegated_authorities
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
