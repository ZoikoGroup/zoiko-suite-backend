-- 0034_authorization_svc_delegation_and_abac.sql
-- authorization-svc → schema `authorization_svc`. Creates one table.
--
-- The changes authorization-svc's own 000008_fix_delegation_evaluation and
-- 000010_add_abac_rules make, in the form this project applies them. Same end
-- state; whichever runs first, the other is a no-op.
--
-- ── Part 1: delegated access has granted nothing since 0031 ─────────────────
--
-- 0031 gave delegated_authorities row security and said, deliberately:
--
--   "No platform_scope hatch, unlike roles: nothing needs to discover which
--    tenant owns an unknown delegation_id."
--
-- The reasoning behind that — quoting the service's own 000006 — was that
-- "FindDelegatedActions is reached from /v1/authorize which resolves one". That
-- second half is false: resolveTenantScope has a no-tenant branch and warns on
-- every use of it. On that branch the service runs under app.platform_scope,
-- which `roles`, `permission_bundles` and `principal_role_assignments` all
-- honour (0028 relies on exactly this) — and which this table did not.
-- app_authorization is NOSUPERUSER NOBYPASSRLS, so the policy binds, and the
-- delegation lookup matched zero rows.
--
-- To be exact about what this half fixes: on a deployment with the canonical
-- input-contract middleware at its default (write-strict) a tenantless POST is
-- refused with 401 before the handler runs, so the SERVICE-side fix — routing
-- the query through withRLS/withPlatformScope instead of the bare pool — is
-- what restores delegated access for callers that get through. This hatch
-- covers observe mode, in which the branch is reachable, and makes the store's
-- documented "empty tenant evaluates across tenants" contract true instead of
-- silently empty.
--
-- It failed CLOSED, which is why nothing broke visibly: a delegate was denied
-- with basis `no_grant`, indistinguishable from having no delegation at all.
--
-- Measured on Postgres 16 as a NOBYPASSRLS role, one ACTIVE, in-date,
-- correctly-tenanted delegation present:
--
--   no tenant, no platform scope   -> 0 rows   (the behaviour being fixed)
--   tenant installed               -> 1 row
--   platform scope only            -> 0 rows   (no hatch to honour)
--
-- The hatch below changes the third line. USING only, never WITH CHECK — a
-- read may have to resolve cross-tenant on the /v1/authorize path, but nothing
-- may legitimately WRITE a delegation outside the caller's verified tenant, and
-- CreateDelegatedAuthority goes through the handler's requireTenant first.
-- Same asymmetry 0028 uses.
--
-- The hatch grants visibility, not authority: the service's own
-- FindDelegatedActions query binds each delegator's roles to the delegation's
-- OWN tenant_id, so platform-wide visibility does not become platform-wide
-- grant resolution.
--
-- ── Part 1b: delegated_actions, and an over-grant it closes ─────────────────
--
-- scope_type has always accepted 'ACTION_SUBSET' and authority_limit_type /
-- authority_limit_value have always been stored, and NOTHING ever read any of
-- them: the evaluation unioned the delegator's entire effective grant set
-- regardless. A delegation recorded as a subset therefore conferred the
-- delegator's FULL authority — silent, because the row looks correctly
-- restricted in the register.
--
-- delegated_actions is what the evaluation intersects against. NULL means the
-- delegator's full authority, which is what every existing row means, so no
-- backfill is needed and no row's meaning changes.
--
-- source_service / source_delegation_id are where the AUTHORITATIVE Delegated
-- Authority Service's authority.delegated events land. Doc 03 §9.3 names that
-- service as the owner of this concept (tracker item 81), and this is how the
-- two stop being rival write models: it stays authoritative for the lifecycle,
-- and this table becomes the evaluation read-model /v1/authorize resolves
-- against. It delegates ONE action_type per grant, which has no representation
-- here until delegated_actions exists.
--
-- ── Part 2: abac_rules ──────────────────────────────────────────────────────
--
-- The attribute-condition layer the spec assigns this service. The table ships
-- EMPTY: every concrete rule is a business decision, and with no rows the layer
-- is a no-op. What is added is the mechanism, not a guess at the policy — the
-- same shape sod_rules already has.
--
-- Deny-only by construction: a rule can remove an action the RBAC/delegation
-- layers granted and can never add one.

DO $guard$
BEGIN

IF NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'authorization_svc') THEN
    RAISE NOTICE 'schema authorization_svc absent; skipping 0034 — re-run it after deployments/supabase has created the schema';
    RETURN;
END IF;

-- ── Part 1: the platform-scope read hatch ───────────────────────────────────
--
-- app.current_tenant_id() rather than current_setting directly, matching 0031:
-- on this database a caller may arrive through PostgREST with a JWT instead of
-- through a service calling set_config, and only the helper resolves both.
--
-- app.platform_scope IS read with current_setting, because it is never a JWT
-- claim — it is set only by PgStore.withPlatformScope, inside one transaction,
-- and there is no equivalent for a PostgREST caller by design. A JWT holder
-- must not be able to claim platform scope.
EXECUTE $stmt$DROP POLICY IF EXISTS tenant_isolation_policy ON authorization_svc.delegated_authorities$stmt$;
EXECUTE $stmt$
CREATE POLICY tenant_isolation_policy ON authorization_svc.delegated_authorities
    FOR ALL
    USING (
        tenant_id::text = app.current_tenant_id()
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id::text = app.current_tenant_id())
$stmt$;

-- ── Part 1b: the subset and projection columns ──────────────────────────────

EXECUTE $stmt$ALTER TABLE authorization_svc.delegated_authorities ADD COLUMN IF NOT EXISTS delegated_actions JSONB$stmt$;
EXECUTE $stmt$ALTER TABLE authorization_svc.delegated_authorities ADD COLUMN IF NOT EXISTS source_service TEXT$stmt$;
EXECUTE $stmt$ALTER TABLE authorization_svc.delegated_authorities ADD COLUMN IF NOT EXISTS source_delegation_id TEXT$stmt$;

EXECUTE $stmt$
COMMENT ON COLUMN authorization_svc.delegated_authorities.delegated_actions IS
    'JSON array of action codes this delegation confers, intersected with the delegator''s own effective grants at evaluation time. NULL means the delegator''s full authority (the meaning of every row written before this column existed). A delegation can never confer an action the delegator does not hold.'
$stmt$;

-- UNIQUE where present, not a primary key: locally-authored rows have no
-- upstream id and must stay insertable, and a partial index is how "unique
-- when present" is expressed. It is what the projection's ON CONFLICT targets,
-- so without it a Kafka redelivery would multiply one upstream delegation into
-- several rows that /v1/authorize would union into a duplicate grant.
EXECUTE $stmt$
CREATE UNIQUE INDEX IF NOT EXISTS idx_delegations_source_unique
    ON authorization_svc.delegated_authorities (source_service, source_delegation_id)
    WHERE source_delegation_id IS NOT NULL
$stmt$;

-- ── Part 2: abac_rules ──────────────────────────────────────────────────────

EXECUTE $stmt$
CREATE TABLE IF NOT EXISTS authorization_svc.abac_rules (
    abac_rule_id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID,
    rule_code                VARCHAR(128) NOT NULL,
    action_type              VARCHAR(128) NOT NULL,
    effect                   VARCHAR(16)  NOT NULL,
    attribute_key            VARCHAR(128) NOT NULL,
    operator                 VARCHAR(32)  NOT NULL,
    attribute_value          TEXT,
    active_flag              BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT         NOT NULL
)
$stmt$;

-- Two partial indexes rather than one UNIQUE(tenant_id, rule_code): NULLs are
-- distinct in a Postgres unique index, so a single constraint would let the
-- same PLATFORM-WIDE rule_code be created any number of times.
EXECUTE $stmt$
CREATE UNIQUE INDEX IF NOT EXISTS idx_abac_rules_tenant_code_unique
    ON authorization_svc.abac_rules (tenant_id, rule_code) WHERE tenant_id IS NOT NULL
$stmt$;
EXECUTE $stmt$
CREATE UNIQUE INDEX IF NOT EXISTS idx_abac_rules_global_code_unique
    ON authorization_svc.abac_rules (rule_code) WHERE tenant_id IS NULL
$stmt$;
EXECUTE $stmt$
CREATE INDEX IF NOT EXISTS idx_abac_rules_action
    ON authorization_svc.abac_rules (action_type) WHERE active_flag
$stmt$;

EXECUTE $stmt$ALTER TABLE authorization_svc.abac_rules ENABLE ROW LEVEL SECURITY$stmt$;
EXECUTE $stmt$ALTER TABLE authorization_svc.abac_rules FORCE  ROW LEVEL SECURITY$stmt$;

-- sod_rules' shape from 0028, and for the same reasons. A NULL-tenant rule is
-- platform-wide and must be visible in every scope; the WITH CHECK admits NULL
-- only for a caller with NO tenant, so one tenant cannot author a rule that
-- binds every other one. Authoring a platform-wide rule is gated in the
-- handler by a distinct platform-scope grant
-- (handler.ActionABACRuleManageGlobal).
EXECUTE $stmt$DROP POLICY IF EXISTS tenant_isolation_policy ON authorization_svc.abac_rules$stmt$;
EXECUTE $stmt$
CREATE POLICY tenant_isolation_policy ON authorization_svc.abac_rules
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id::text = app.current_tenant_id()
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id::text = app.current_tenant_id())
        OR (tenant_id IS NULL AND app.current_tenant_id() IS NULL)
    )
$stmt$;

-- The app role needs DML on the new table, and on this project the grants are
-- per-role rather than from a script. Guarded because the role name differs
-- between a Supabase project (app_authorization) and a bare Postgres one.
IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
    EXECUTE $stmt$GRANT SELECT, INSERT, UPDATE, DELETE ON authorization_svc.abac_rules TO app_authorization$stmt$;
END IF;
IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_backend') THEN
    EXECUTE $stmt$GRANT SELECT, INSERT, UPDATE, DELETE ON authorization_svc.abac_rules TO zoiko_backend$stmt$;
END IF;

-- ── Verification ────────────────────────────────────────────────────────────

DECLARE unprotected int; seeded int;
BEGIN
    SELECT count(*) INTO unprotected
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'authorization_svc' AND c.relkind = 'r'
       AND NOT (c.relrowsecurity AND c.relforcerowsecurity);
    IF unprotected > 0 THEN
        RAISE EXCEPTION
            '% authorization_svc tables lack forced row security after 0034 — abac_rules was meant to arrive with it', unprotected;
    END IF;

    -- The layer must ship as a no-op. A seeded rule here would be this
    -- migration declaring business policy, which is precisely what the design
    -- refuses to do.
    SELECT count(*) INTO seeded FROM authorization_svc.abac_rules;
    IF seeded > 0 THEN
        RAISE EXCEPTION
            'abac_rules holds % rows immediately after creation — the ABAC layer must ship empty so it changes no decision until somebody declares a rule', seeded;
    END IF;

    RAISE NOTICE '0034 applied: delegated access can resolve for tenantless callers, action subsets are representable, abac_rules exists and is empty.';
END;

END
$guard$;
