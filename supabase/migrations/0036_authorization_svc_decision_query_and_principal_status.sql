-- 0036_authorization_svc_decision_query_and_principal_status.sql
-- authorization-svc → schema `authorization_svc`. Adds one index swap and one
-- table.
--
-- Mirrors the service's own 000012_index_access_decision_log_for_query and
-- 000013_add_principal_status_projection. Same end state; the policies are this
-- project's (app.current_tenant_id()) rather than the compose one's
-- current_setting('app.tenant_id').
--
-- ── PART 1 — MAKING THE DECISION LOG READABLE ───────────────────────────────
--
-- Doc 03 §8.3 sets two evidence obligations on this service: "every decision
-- logged with actor, action, basis, and outcome", and "denials must be
-- evidentially retrievable". The first held from 000001. The second did not,
-- and the gap was not in the schema — it was that the only read path was a
-- primary-key lookup, which is retrieval only for somebody who already holds
-- the key. A denial's key exists in exactly one place: the response returned to
-- the service that was refused. So auditing a denial meant reading the CALLING
-- service's logs to find a UUID to hand back to this one.
--
-- GET /v1/access-decisions is the read that closes it, and this index is what
-- makes it affordable on the largest table on the platform.
--
-- 000009 created (tenant_id, decided_at DESC), which already serves an
-- unfiltered tenant listing. What it does not serve is the query the obligation
-- actually names: DENIALS. decision_outcome is 'GRANTED' for the overwhelming
-- majority of rows, so filtering on DENIED through the tenant index means
-- scanning predominantly non-matching entries at exactly the moment somebody is
-- answering an auditor.
--
-- (tenant_id, decision_outcome, decided_at DESC) serves both — the outcome
-- filter by equality in the middle column, the unfiltered listing by the leading
-- prefix. The old index is a strict PREFIX of the new one, so it is DROPPED
-- rather than kept: index count matters more on this table than anywhere else
-- on the platform, because it takes one INSERT per authorization evaluation and
-- every index is maintained on every one of them. Replacing a prefix is
-- index-count-neutral; adding a fifth index would have charged the whole
-- platform's write latency for a read that runs when an auditor asks.
--
-- ── PART 2 — WHETHER THE PRINCIPAL IS STILL A PRINCIPAL ─────────────────────
--
-- §8.3: "Determines whether a principal may execute a specific action in a
-- given tenant, entity, jurisdiction, and workflow state." A principal that
-- identity-context-svc has SUSPENDED or DISABLED may execute nothing. Until
-- 000013 this service had no way to know that, so a suspended principal kept
-- every grant they held and every action those grants permitted.
--
-- The obvious objection is that identity-context-svc already evicts SESSIONS on
-- principal.status.changed, so a suspended human cannot log in. True, and not
-- the same control: this endpoint is called east-west, service to service, on
-- requests whose identity envelope was resolved before the suspension, and by
-- scheduled and queued work that carries a principal and no session at all.
-- Session eviction closes the front door; nothing closed this one.
--
-- ── WHY principal.status.changed AND NOT employee.terminated ────────────────
--
-- §8.3 names `employment.changed` as a consumed event, and four passes of this
-- service's notes recorded it as having no producer. That was a grep for the
-- SPEC's name. The platform publishes the concept under concrete ones, and they
-- are not interchangeable:
--
--   principal.status.changed  identity-context-svc, zoiko.identity.events.
--                             Payload: principal_id, tenant_id, new_status.
--                             CONSUMED — keyed on principal_id, which is
--                             exactly what principal_role_assignments is keyed
--                             on.
--   employee.terminated       employee-master-svc AND offboarding-severance-svc,
--                             zoiko.employee.events. Payload names an
--                             employee_id, and there is NO employee-to-principal
--                             mapping anywhere on this estate —
--                             employee-master-svc's schema carries no
--                             principal_id or user_id column at all. So the
--                             event says somebody left and this service cannot
--                             tell whose grants that is about. Consuming it on a
--                             guessed join would end the wrong principal's
--                             authority, silently.
--
-- ── A PROJECTION, NOT A REVOCATION ──────────────────────────────────────────
--
-- The tempting implementation is to end the principal's role assignments on
-- suspension. That is wrong in a way worth stating: SUSPENDED is reversible and
-- effective_to is not. Ending the assignments would mean an unsuspension
-- restored nothing and somebody would have to reconstruct by hand what the
-- principal used to hold. This table records the STATUS; the assignments are
-- untouched; a reinstatement is one more projected row.
--
-- ── ABSENCE MEANS ACTIVE, DELIBERATELY ──────────────────────────────────────
--
-- No row means ACTIVE, so the table SHIPS EMPTY and changes no outcome until a
-- status event arrives — the same shape abac_rules ships in (0034).
--
-- That is a fail-OPEN default in a service that fails closed everywhere else,
-- and it is the correct one here: the alternative denies every principal on the
-- platform until a status event happens to arrive for them, which is a total
-- outage of the authorization plane on first deploy. The gate can only ever take
-- access away from a principal an authoritative service has explicitly said is
-- not active. Deny-only, on the same terms as ABAC.
--
-- ── IDEMPOTENT ──────────────────────────────────────────────────────────────
--
-- Additive and re-runnable: the index swap checks what exists before acting, and
-- the table is CREATE TABLE IF NOT EXISTS. Also skips cleanly when the schema is
-- absent, so a project where deployments/supabase has not yet created it sees a
-- notice rather than an error — same guard 0035 carries.

DO $guard$
BEGIN

IF NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'authorization_svc') THEN
    RAISE NOTICE 'schema authorization_svc absent; skipping 0036 — re-run it after deployments/supabase has created the schema';
    RETURN;
END IF;

-- ── Part 1: the outcome-aware index ─────────────────────────────────────────

-- Created on the PARENT, which propagates to every existing partition and to
-- every partition created later — including by
-- authorization_svc.create_access_decision_log_partition(), so a new month is
-- never briefly unindexed. Same mechanism 0035 relies on.
IF NOT EXISTS (
    SELECT 1 FROM pg_indexes
     WHERE schemaname = 'authorization_svc'
       AND indexname  = 'idx_access_decision_log_tenant_outcome'
) THEN
    EXECUTE $stmt$
        CREATE INDEX idx_access_decision_log_tenant_outcome
            ON authorization_svc.access_decision_log (tenant_id, decision_outcome, decided_at DESC)
    $stmt$;
    RAISE NOTICE '0036: idx_access_decision_log_tenant_outcome created';
ELSE
    RAISE NOTICE '0036: idx_access_decision_log_tenant_outcome already present';
END IF;

EXECUTE $stmt$
    COMMENT ON INDEX authorization_svc.idx_access_decision_log_tenant_outcome IS
        'Serves GET /v1/access-decisions: tenant-scoped listings ordered by decided_at DESC, with or without a decision_outcome filter. Supersedes idx_access_decision_log_tenant, of which it is a superset.'
$stmt$;

-- Dropped only AFTER its superset exists, so there is never an instant where a
-- tenant-scoped listing has no index to use.
IF EXISTS (
    SELECT 1 FROM pg_indexes
     WHERE schemaname = 'authorization_svc'
       AND indexname  = 'idx_access_decision_log_tenant'
) THEN
    EXECUTE 'DROP INDEX authorization_svc.idx_access_decision_log_tenant';
    RAISE NOTICE '0036: idx_access_decision_log_tenant dropped — superseded';
END IF;

-- ── Part 2: principal_status_projection ─────────────────────────────────────

CREATE TABLE IF NOT EXISTS authorization_svc.principal_status_projection (
    -- TEXT, matching principal_role_assignments.principal_id. Not UUID: this
    -- service has never required a principal id to be one, and a service
    -- account id that is not a UUID must still be suspendable.
    principal_id       TEXT         NOT NULL,

    -- NOT NULL. A tenantless status row could not be scoped by any policy, and
    -- a status applying to every tenant is not something identity-context-svc
    -- can express.
    tenant_id          UUID         NOT NULL,

    -- ACTIVE | SUSPENDED | DISABLED — identity-context-svc's own
    -- PrincipalStatus values, carried verbatim. Data only, VARCHAR not enum,
    -- same doctrine as role_scope_type: a status this build has never seen is
    -- stored and treated as not-ACTIVE by the gate, which is the closed
    -- direction.
    status             VARCHAR(32)  NOT NULL,

    source_service     TEXT         NOT NULL,

    -- When upstream says the change happened. Nullable: the current payload
    -- does not carry it, and inventing NOW() would make a replayed old event
    -- look like a fresh change.
    status_changed_at  TIMESTAMPTZ,

    projected_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    PRIMARY KEY (tenant_id, principal_id)
);

COMMENT ON TABLE authorization_svc.principal_status_projection IS
    'Read-model of identity-context-svc''s principal status, consumed from principal.status.changed on zoiko.identity.events. Evaluated as layer 0 of POST /v1/authorize: a principal whose projected status is not ACTIVE is denied every action. NO ROW MEANS ACTIVE — the table ships empty and changes no outcome until a status event arrives. Never written by this service''s admin API; identity-context-svc is authoritative.';

COMMENT ON COLUMN authorization_svc.principal_status_projection.status IS
    'identity-context-svc''s PrincipalStatus verbatim: ACTIVE | SUSPENDED | DISABLED. Any value other than ACTIVE denies — an unrecognised status is treated as not-active, which is the fail-closed direction.';

-- The gate's lookup when the caller supplied no tenant: by principal alone,
-- most-restrictive-first. PARTIAL, because the only rows this index has to find
-- are the ones that deny — an ACTIVE row and an absent row have the same
-- effect, so indexing the ACTIVE rows would pay for entries that can never
-- change an outcome.
CREATE INDEX IF NOT EXISTS idx_principal_status_not_active
    ON authorization_svc.principal_status_projection (principal_id)
    WHERE status <> 'ACTIVE';

ALTER TABLE authorization_svc.principal_status_projection ENABLE ROW LEVEL SECURITY;
ALTER TABLE authorization_svc.principal_status_projection FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON authorization_svc.principal_status_projection;

-- app.current_tenant_id() rather than current_setting directly, matching 0031
-- and 0034.
--
-- The `app.current_tenant_id() IS NULL` escape in USING is this project's form
-- of the app.platform_scope hatch the service's own 000013 carries, and it is
-- there for one specific caller: FindPrincipalStatus with no tenant, which is
-- 86 of this endpoint's 111 callers today. Without it a tenantless read sees
-- zero rows, reads that as ACTIVE, and the gate is bypassed by omitting a
-- header — which would make this whole migration decorative for most of the
-- platform.
--
-- WITH CHECK deliberately carries NO escape. Every write comes from the event
-- consumer, which has a tenant from the envelope or refuses to project at all,
-- so nothing may legitimately write outside a named tenant.
CREATE POLICY tenant_isolation_policy ON authorization_svc.principal_status_projection
    FOR ALL
    USING (
        app.current_tenant_id() IS NULL
        OR tenant_id::text = app.current_tenant_id()
    )
    WITH CHECK (tenant_id::text = app.current_tenant_id());

-- ── Verification ────────────────────────────────────────────────────────────

DECLARE unprotected int;
BEGIN
    -- Every table and partition in the schema must carry forced row security.
    -- One without it is an unprotected copy of a protected table, and on this
    -- project PostgREST can reach it.
    SELECT count(*) INTO unprotected
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'authorization_svc'
       AND c.relkind IN ('r', 'p')
       AND NOT (c.relrowsecurity AND c.relforcerowsecurity);
    IF unprotected > 0 THEN
        RAISE EXCEPTION
            '% authorization_svc tables or partitions lack forced row security after 0036', unprotected;
    END IF;

    RAISE NOTICE '0036 applied: the decision log is searchable by outcome, and principal_status_projection is layer 0 of every authorization decision (empty, so inert until identity publishes a status change).';
END;

END
$guard$;
