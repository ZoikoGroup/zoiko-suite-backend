-- Migration: 000008_fix_delegation_evaluation.up.sql
--
-- Two changes to delegated_authorities, both needed before layer 2 of
-- /v1/authorize (delegated access) grants anything at all.
--
-- ── 1. The platform-scope read hatch ────────────────────────────────────────
--
-- 000006 gave this table RLS with no app.platform_scope escape, and said so
-- deliberately: "Nothing needs to discover which tenant owns an unknown
-- delegation_id: every caller already knows its own tenant, and
-- FindDelegatedActions is reached from /v1/authorize which resolves one."
--
-- The second half of that sentence is false. /v1/authorize resolves a tenant
-- scope that is legitimately EMPTY — resolveTenantScope has a no-tenant branch
-- and warns on every use of it — and FindGrantedActions handles that branch by
-- running under app.platform_scope, which roles / permission_bundles /
-- principal_role_assignments all honour (000004, 000007). This table did not,
-- so the identical branch on the delegation lookup matched zero rows.
--
-- Measured on Postgres 16.15 as a NOSUPERUSER NOBYPASSRLS role, one ACTIVE,
-- in-date, correctly-tenanted delegation present:
--
--   no app.tenant_id, no platform scope   -> 0 rows   (today's behaviour)
--   app.tenant_id = the delegation's      -> 1 row
--   app.platform_scope = 'true' only      -> 0 rows   (no hatch to honour)
--
-- The third line is what this half of the migration changes. Same shape as
-- 000007's: the hatch is carried in USING only, never in WITH CHECK — a read
-- may have to resolve cross-tenant on the /v1/authorize path, but nothing in
-- this service may legitimately WRITE a delegation outside the caller's
-- verified tenant, and CreateDelegatedAuthority goes through
-- handler.requireTenant first.
--
-- The hatch grants visibility, not authority. PgStore.FindDelegatedActions is
-- the only reader that reaches it, it is reached only when the caller supplied
-- no tenant at all, and the query it runs binds each delegator's roles to the
-- delegation's OWN tenant_id (see part 2 and the store) — so platform-wide
-- visibility does not become platform-wide grant resolution.
--
-- ── WHICH HALF IS LOAD-BEARING TODAY ────────────────────────────────────────
--
-- Stated precisely, because the first version of this comment overstated it.
-- The tenantless branch is legitimate and documented in the store, but on a
-- DEFAULT deployment it is not reachable over HTTP: the canonical
-- input-contract middleware (ZS_ENVELOPE_ENFORCEMENT=write-strict, the
-- default) treats tenant_id as unconditionally mandatory and answers 401
-- before the handler runs. Verified against the running service.
--
-- So of the two fixes, routing the query through withRLS is the one that
-- restores delegated access for every caller that gets through today. This
-- hatch matters for three reasons that are not hypothetical:
--
--   * observe mode is a documented migration state in which the branch IS
--     reachable, and in that mode delegation would otherwise still grant
--     nothing;
--   * FindDelegatedActions' own contract says an empty tenant evaluates
--     across tenants, and without the hatch that contract silently returns
--     nothing instead — a lie in the store's documentation, which is how the
--     original bug survived review;
--   * it is the same shape roles / permission_bundles /
--     principal_role_assignments already have (000004, 000007), so the four
--     tables the evaluation joins now behave identically instead of one
--     failing closed on its own.
--
-- ── 2. delegated_actions ────────────────────────────────────────────────────
--
-- scope_type has always accepted 'ACTION_SUBSET' and authority_limit_type /
-- authority_limit_value have always been stored, and NOTHING has ever read any
-- of them: FindDelegatedActions unions the delegator's entire effective grant
-- set regardless. A delegation recorded as a subset therefore conferred the
-- delegator's FULL authority — an over-grant on the platform's authorization
-- path, silent because the row looks correctly restricted in the register.
--
-- delegated_actions is the column the evaluation can actually intersect
-- against. NULL means "the delegator's full authority", which is what every
-- existing row means today, so this column changes no existing row's meaning
-- and needs no backfill. A JSON array names the subset.
--
-- It is also where the authoritative Delegated Authority Service's
-- authority.delegated events land: that service delegates ONE action_type per
-- grant (see its publisher's payload), which has no representation in this
-- table until this column exists.

BEGIN;

-- ── 1 ───────────────────────────────────────────────────────────────────────

DROP POLICY IF EXISTS tenant_isolation_policy ON delegated_authorities;
CREATE POLICY tenant_isolation_policy ON delegated_authorities
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ── 2 ───────────────────────────────────────────────────────────────────────

ALTER TABLE delegated_authorities
    ADD COLUMN IF NOT EXISTS delegated_actions JSONB;

COMMENT ON COLUMN delegated_authorities.delegated_actions IS
    'JSON array of action codes this delegation confers, intersected with the delegator''s own effective grants at evaluation time. NULL means the delegator''s full authority (the meaning of every row written before this column existed). A delegation can never confer an action the delegator does not hold.';

-- Provenance for a row projected from the authoritative Delegated Authority
-- Service rather than written through this service's own admin API. NULL means
-- locally authored. Kept as plain text (a service name), not an enum: same
-- data-only doctrine as scope_type.
ALTER TABLE delegated_authorities
    ADD COLUMN IF NOT EXISTS source_service TEXT;

-- The upstream service's own delegation_id, so a projected row is idempotent
-- on redelivery and can be revoked/expired by a later event naming the same
-- id. UNIQUE, not the primary key: locally-authored rows have no upstream id
-- and must stay insertable, and a partial index is how "unique when present"
-- is expressed.
ALTER TABLE delegated_authorities
    ADD COLUMN IF NOT EXISTS source_delegation_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_delegations_source_unique
    ON delegated_authorities (source_service, source_delegation_id)
    WHERE source_delegation_id IS NOT NULL;

COMMIT;
