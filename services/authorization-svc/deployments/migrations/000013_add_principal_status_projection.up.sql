-- Migration: 000013_add_principal_status_projection.up.sql
--
-- Gives POST /v1/authorize the one input it has never had: whether the
-- principal it is evaluating is still an active principal at all.
--
-- ── WHY, AND WHY IT IS THIS SERVICE'S PROBLEM ───────────────────────────────
--
-- Doc 03 §8.3: "Determines whether a principal may execute a specific action
-- in a given tenant, entity, jurisdiction, and workflow state." A principal
-- that identity-context-svc has SUSPENDED or DISABLED may execute nothing.
-- Until now this service had no way to know that, so a suspended principal
-- kept every grant they held and every action those grants permitted.
--
-- The obvious objection is that identity-context-svc already invalidates
-- SESSIONS on principal.status.changed, so a suspended human cannot log in.
-- That is true and it is not the same control. This endpoint is called
-- east-west, service to service, 111 callers deep, on requests whose identity
-- envelope was resolved before the suspension and on scheduled and queued work
-- that carries a principal but no session at all. Session eviction closes the
-- front door; nothing closed this one.
--
-- ── WHY THIS EVENT AND NOT employee.terminated ──────────────────────────────
--
-- §8.3 names `employment.changed` as a consumed event, and four passes of this
-- service's notes recorded it as having no producer. That was a grep for the
-- SPEC's event name. The platform does publish the concept, under concrete
-- names, and they are not interchangeable:
--
--   principal.status.changed  identity-context-svc, zoiko.identity.events
--                             payload: principal_id, tenant_id, new_status
--                             ← CONSUMED HERE. Keyed on principal_id, which is
--                               exactly the identifier every row in
--                               principal_role_assignments is keyed on.
--
--   employee.terminated       employee-master-svc AND offboarding-severance-svc,
--   employee.status.changed   zoiko.employee.events
--                             payload: employee_id, tenant_id, legal_entity_id
--                             ← NOT consumed, and this is the reason: the
--                               payload names an employee_id, and there is no
--                               employee-to-principal mapping anywhere on this
--                               estate. employee-master-svc's schema carries no
--                               principal_id or user_id column at all. So the
--                               event says somebody left and this service
--                               cannot tell whose grants that is about.
--
-- Consuming employee.terminated on a guessed join would end the wrong
-- principal's authority, silently, which is worse than not consuming it. The
-- gap is recorded in known-gaps.md as an identity-mapping gap rather than as a
-- missing consumer, which is what it actually is.
--
-- ── A PROJECTION, NOT A REVOCATION ──────────────────────────────────────────
--
-- The tempting implementation is to end the principal's role assignments on
-- suspension. That is wrong in a way worth stating: SUSPENDED is reversible and
-- effective_to is not. Ending the assignments would mean an unsuspension
-- restored nothing, and somebody would have to reconstruct by hand what the
-- principal used to hold. This table records the STATUS; the assignments are
-- untouched; unsuspension is one more projected row and everything the
-- principal held is live again.
--
-- ── ABSENCE MEANS ACTIVE, DELIBERATELY ──────────────────────────────────────
--
-- No row for a principal means ACTIVE, so this table SHIPS EMPTY and changes
-- no outcome until identity-context-svc publishes a status change — the same
-- shape abac_rules ships in.
--
-- That is a fail-OPEN default, in a service that fails closed everywhere else,
-- and it is the correct one here: the alternative denies every principal on the
-- platform until a status event happens to arrive for them, which is a total
-- outage of the authorization plane on first deploy. The gate can only ever
-- take access away from a principal an authoritative service has explicitly
-- said is not active. It is a deny-only layer, on the same terms as ABAC.
--
-- ── PRIMARY KEY (tenant_id, principal_id) ───────────────────────────────────
--
-- A principal's status is per tenant because that is how identity-context-svc
-- publishes it — the event carries both, and the same principal can hold
-- standing in more than one tenant. FindPrincipalStatus resolves the
-- restrictive row when a caller supplies no tenant at all (see its comment),
-- so a suspension in one tenant is not silently escapable by omitting a header.

BEGIN;

CREATE TABLE principal_status_projection (
    -- TEXT, matching principal_role_assignments.principal_id. Not UUID: this
    -- service has never required a principal id to be one, and a service
    -- account id that is not a UUID must still be suspendable.
    principal_id       TEXT         NOT NULL,

    -- NOT NULL. A tenantless status row could not be scoped by any policy,
    -- and a status that applies to every tenant is not a thing
    -- identity-context-svc can express.
    tenant_id          UUID         NOT NULL,

    -- ACTIVE | SUSPENDED | DISABLED — identity-context-svc's own
    -- PrincipalStatus values, carried verbatim. Data only, VARCHAR not enum,
    -- same doctrine as role_scope_type: a status this build has never seen is
    -- stored, and treated as not-ACTIVE by the gate, which is the closed
    -- direction.
    status             VARCHAR(32)  NOT NULL,

    -- Which service asserted this. One value today; recorded rather than
    -- assumed so a second producer is visible in the data instead of
    -- indistinguishable from the first.
    source_service     TEXT         NOT NULL,

    -- When the upstream service says the status changed, from the event
    -- payload. Nullable: the payload does not always carry it, and inventing
    -- NOW() would make a replayed old event look like a fresh change.
    status_changed_at  TIMESTAMPTZ,

    -- When this row was written here. Always set, and the field an operator
    -- reads to tell projection lag from upstream silence.
    projected_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    PRIMARY KEY (tenant_id, principal_id)
);

COMMENT ON TABLE principal_status_projection IS
    'Read-model of identity-context-svc''s principal status, consumed from principal.status.changed on zoiko.identity.events. Evaluated as layer 0 of POST /v1/authorize: a principal whose projected status is not ACTIVE is denied every action. NO ROW MEANS ACTIVE — the table ships empty and changes no outcome until a status event arrives. Never written by this service''s admin API; identity-context-svc is authoritative.';

COMMENT ON COLUMN principal_status_projection.status IS
    'identity-context-svc''s PrincipalStatus verbatim: ACTIVE | SUSPENDED | DISABLED. Any value other than ACTIVE denies — an unrecognised status is treated as not-active, which is the fail-closed direction.';

-- The gate's lookup when the caller supplied no tenant: by principal alone,
-- most-restrictive-first. Partial, because the only rows this index has to
-- find are the ones that deny — an ACTIVE row and an absent row have the same
-- effect, so indexing the ACTIVE rows would be paying for entries that can
-- never change an outcome.
CREATE INDEX idx_principal_status_not_active
    ON principal_status_projection (principal_id)
    WHERE status <> 'ACTIVE';

-- ── row level security ──────────────────────────────────────────────────────
--
-- The platform_scope hatch in USING is the same one 000008 added to
-- delegated_authorities and for the same caller: FindPrincipalStatus with no
-- tenant, which is 86 of this endpoint's 111 callers today. Without it, a
-- tenantless call reads zero rows, reads that as ACTIVE, and the gate is
-- bypassed by omitting a header — which would make this migration decorative
-- for most of the platform.
--
-- WITH CHECK deliberately carries NO hatch. Every write here comes from the
-- event consumer, which has a tenant from the envelope or refuses to project at
-- all, so nothing may legitimately write outside a named tenant.
ALTER TABLE principal_status_projection ENABLE ROW LEVEL SECURITY;
ALTER TABLE principal_status_projection FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON principal_status_projection
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
