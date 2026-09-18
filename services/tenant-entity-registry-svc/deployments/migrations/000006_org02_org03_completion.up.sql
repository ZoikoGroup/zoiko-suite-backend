-- 000006_org02_org03_completion.up.sql
--
-- Closes the ORG-02 (Tenant) and ORG-03 (Legal Entity) gaps in
-- ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_
-- Specifications §4.2, §4.3, §8 and §9.2.
--
-- Six things, each of which the spec names and none of which existed:
--
--   1. legal_entity_profile_versions — ORG-03's central requirement. Today
--      UpdateEntity mutates legal_entities in place, so a legal-name change
--      destroys the name the financial history was booked under. §4.3 requires
--      effective-dated profile versions and "as-of entity reconstruction
--      exact"; §8 NP6 requires that a name change after financial history
--      exists resolves the ORIGINAL version for historical reads. That is not
--      expressible against a single mutable row.
--
--   2. tenant_lifecycle_history — ORG-02 names ListTenantLifecycleHistory as a
--      read surface and requires "durable lifecycle evidence" with
--      "lifecycle actor/reason". The transition currently overwrites
--      tenants.lifecycle_state and keeps no record of what it was.
--
--   3. tenant_host_bindings — ORG-02 names ResolveTenantByHost, and §8 NP3
--      ("host resolves to tenant A but body/header claims tenant B → reject
--      before data access") is not merely untested without it, it is
--      untestable: there is no host→tenant mapping in this service to disagree
--      with the header.
--
--   4. entity_registry_conflicts — §8 NP5 requires that the same registry
--      number claimed by two active entities in the same jurisdiction is
--      QUARANTINED, explicitly "no silent merge". A quarantine needs somewhere
--      to put the thing being quarantined.
--
--   5. event_outbox — §9.2 requires the authoritative write path to use a
--      transactional outbox. Today every publish is `go s.events.Publish...`,
--      a goroutine on a detached context after the transaction has already
--      committed: a crash or broker blip in that window leaves the database
--      holding a fact the estate never hears about. Same table shape and same
--      named-capability RLS escape hatch as identity-context-svc's
--      000007 event_outbox, deliberately, so the relay pattern is one pattern.
--
--   6. record_version — §4.2 and §4.3 both require expected_version on
--      commands ("lifecycle commands use expected_version", "commands use UUID
--      and expected_version"). Without it two concurrent amendments silently
--      last-write-wins.
--
-- Plus FORCE ROW LEVEL SECURITY on the seven pre-existing tables, which is
-- backend-completion-tracker.md Priority 3 row 56. The runtime role is
-- `zoiko_app` (NOSUPERUSER NOBYPASSRLS) and is deliberately not the table
-- owner, so FORCE changes nothing for it today. It closes the case where a
-- migration or a future ops action runs as the owner: the owner is exempt from
-- its own policies unless FORCE is set.

-- ---------------------------------------------------------------------------
-- 0. Optimistic concurrency — expected_version
-- ---------------------------------------------------------------------------
--
-- Starts at 1 for existing rows and is incremented by every guarded write.
-- A command carrying expected_version N updates WHERE record_version = N; zero
-- rows affected means someone else moved it first, which is a 409, not a
-- silent overwrite of their change.

ALTER TABLE tenants        ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE legal_entities ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

-- ---------------------------------------------------------------------------
-- 1. ORG-03 — legal entity profile versions (bitemporal)
-- ---------------------------------------------------------------------------
--
-- Bitemporal in the sense §9.1 requires: effective_from/effective_to is when
-- the fact was true in the business world, recorded_at is when this platform
-- learned it. The two differ whenever a change is backdated, and §9.2 requires
-- as-of retrieval to be "verified against correction and late-arriving-change
-- scenarios" — which is exactly the case where they differ.
--
-- effective_to NULL means "still in force". A correction supersedes rather
-- than rewrites: superseded_at is set, the row stays, and a historical read
-- that was resolved before the correction can still be reproduced.

CREATE TABLE legal_entity_profile_versions (
    profile_version_id      UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL REFERENCES tenants(tenant_id),
    legal_entity_id         UUID NOT NULL REFERENCES legal_entities(legal_entity_id),

    -- Monotonic per entity. Human-facing ("version 3 of this entity's
    -- profile") and the tiebreaker when two versions share an effective date.
    version_number          INT NOT NULL,

    -- The versioned profile itself. These are the ORG-03 fields §4.3 lists as
    -- requiring effective-dating and evidence: legal name, legal form,
    -- registry identity, formation jurisdiction, registered office.
    legal_name              VARCHAR(255) NOT NULL,
    trading_name            VARCHAR(255),
    legal_form_code         VARCHAR(50),
    legal_form_source       VARCHAR(50),
    legal_form_local_text   VARCHAR(255),
    registration_number     VARCHAR(255),
    registry_authority      VARCHAR(255),
    registered_office       JSONB,
    incorporation_jurisdiction_id UUID,
    default_currency_code   VARCHAR(3),

    -- Business time.
    effective_from          TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to            TIMESTAMP WITH TIME ZONE,

    -- Record time.
    recorded_at             TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    superseded_at           TIMESTAMP WITH TIME ZONE,

    -- §4.3 evidence/lineage: source refs, actor, reason, approver.
    change_reason           VARCHAR(50) NOT NULL,
    source_evidence_ref     VARCHAR(255),
    created_by_principal_id VARCHAR(255) NOT NULL,
    approved_by_principal_id VARCHAR(255),

    CONSTRAINT lepv_version_positive   CHECK (version_number > 0),
    CONSTRAINT lepv_interval_ordered   CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT lepv_entity_version_uq  UNIQUE (legal_entity_id, version_number)
);

-- The as-of query: newest version whose interval contains the business date.
CREATE INDEX idx_lepv_asof
    ON legal_entity_profile_versions (legal_entity_id, effective_from DESC, version_number DESC);

-- The ListEntityVersions query.
CREATE INDEX idx_lepv_entity
    ON legal_entity_profile_versions (legal_entity_id, version_number DESC);

-- ORG-03 read surface FindByRegistryNumber, and the NP5 duplicate probe.
CREATE INDEX idx_lepv_registry
    ON legal_entity_profile_versions (tenant_id, registration_number)
    WHERE registration_number IS NOT NULL;

ALTER TABLE legal_entity_profile_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE legal_entity_profile_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON legal_entity_profile_versions
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- ---------------------------------------------------------------------------
-- 2. ORG-02 — tenant lifecycle history
-- ---------------------------------------------------------------------------

CREATE TABLE tenant_lifecycle_history (
    lifecycle_event_id      UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL REFERENCES tenants(tenant_id),

    from_state              VARCHAR(50),
    to_state                VARCHAR(50) NOT NULL,

    -- The named command that caused it — ActivateTenant, SuspendTenant and so
    -- on. Stored rather than inferred from the state pair, because §4.2's DoD
    -- gate is that no generic path bypasses the named commands: a row here
    -- with a command name nobody named is the evidence that one did.
    command_name            VARCHAR(64) NOT NULL,
    reason                  TEXT NOT NULL,

    actor_principal_id      VARCHAR(255) NOT NULL,
    -- Maker-checker: §4.2 requires creation/termination and home-region
    -- changes to be independently approved in controlled environments.
    approved_by_principal_id VARCHAR(255),
    correlation_id          VARCHAR(255),
    occurred_at             TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    CONSTRAINT tlh_no_self_approval CHECK (
        approved_by_principal_id IS NULL
        OR approved_by_principal_id <> actor_principal_id
    )
);

CREATE INDEX idx_tlh_tenant ON tenant_lifecycle_history (tenant_id, occurred_at DESC);

ALTER TABLE tenant_lifecycle_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_lifecycle_history FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON tenant_lifecycle_history
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- ---------------------------------------------------------------------------
-- 3. ORG-02 — host bindings (ResolveTenantByHost, and §8 NP3)
-- ---------------------------------------------------------------------------
--
-- Deliberately WITHOUT row-level security, and the reason matters.
--
-- This is the one table read BEFORE a tenant is known — resolving a hostname
-- is how the tenant gets established in the first place. An RLS policy keyed
-- on app.tenant_id would require the answer as input to produce the answer.
-- Same reasoning identity-context-svc applies to tenant_ingress_bindings, and
-- recorded here for the same reason: an unexplained table without RLS looks
-- like an oversight in an estate where every other table has it.
--
-- What keeps it safe is that it holds no tenant DATA: a hostname and the id it
-- maps to. Knowing that host X belongs to tenant Y grants nothing — every
-- subsequent read is still scoped by the gateway-verified header.

CREATE TABLE tenant_host_bindings (
    host_binding_id         UUID PRIMARY KEY,
    -- Lowercased at write time; the unique index is what makes two tenants
    -- claiming one hostname a constraint violation rather than a race.
    hostname                VARCHAR(253) NOT NULL UNIQUE,
    tenant_id               UUID NOT NULL REFERENCES tenants(tenant_id),
    is_primary              BOOLEAN NOT NULL DEFAULT FALSE,
    active_flag             BOOLEAN NOT NULL DEFAULT TRUE,
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL,

    CONSTRAINT thb_hostname_lowercase CHECK (hostname = lower(hostname)),
    CONSTRAINT thb_hostname_nonempty  CHECK (length(hostname) > 0)
);

CREATE INDEX idx_thb_tenant ON tenant_host_bindings (tenant_id);

-- At most one primary hostname per tenant.
CREATE UNIQUE INDEX idx_thb_one_primary
    ON tenant_host_bindings (tenant_id)
    WHERE is_primary AND active_flag;

-- ---------------------------------------------------------------------------
-- 4. ORG-03 — registry conflict quarantine (§8 NP5)
-- ---------------------------------------------------------------------------
--
-- §8 NP5's required result is "Quarantine duplicate conflict; no silent
-- merge". Two readings were possible and the stricter one is implemented:
-- the incoming entity is NOT written and the attempt is recorded here for a
-- human to resolve. Writing it and flagging it would leave two active entities
-- holding one registry identity in the authoritative table, which is the state
-- the negative path exists to prevent.

CREATE TABLE entity_registry_conflicts (
    conflict_id             UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL REFERENCES tenants(tenant_id),

    registration_number     VARCHAR(255) NOT NULL,
    jurisdiction_id         UUID NOT NULL,

    -- The entity that already holds this registry identity.
    existing_legal_entity_id UUID NOT NULL REFERENCES legal_entities(legal_entity_id),

    -- The rejected claim, kept whole. There is no row to point at, because the
    -- point is that it was not written.
    attempted_payload       JSONB NOT NULL,

    status                  VARCHAR(30) NOT NULL DEFAULT 'OPEN',
    resolution_note         TEXT,
    resolved_by_principal_id VARCHAR(255),
    resolved_at             TIMESTAMP WITH TIME ZONE,

    detected_at             TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    detected_by_principal_id VARCHAR(255) NOT NULL,
    correlation_id          VARCHAR(255),

    CONSTRAINT erc_status_known CHECK (status IN ('OPEN', 'RESOLVED_DISTINCT', 'RESOLVED_DUPLICATE', 'DISMISSED')),
    -- A resolved conflict must say who resolved it and when; an open one must
    -- not pretend to have been.
    CONSTRAINT erc_resolution_complete CHECK (
        (status = 'OPEN'  AND resolved_by_principal_id IS NULL AND resolved_at IS NULL)
        OR
        (status <> 'OPEN' AND resolved_by_principal_id IS NOT NULL AND resolved_at IS NOT NULL)
    )
);

CREATE INDEX idx_erc_open
    ON entity_registry_conflicts (tenant_id, detected_at DESC)
    WHERE status = 'OPEN';

ALTER TABLE entity_registry_conflicts ENABLE ROW LEVEL SECURITY;
ALTER TABLE entity_registry_conflicts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON entity_registry_conflicts
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- ---------------------------------------------------------------------------
-- 5. Transactional outbox (§9.2)
-- ---------------------------------------------------------------------------

CREATE TABLE event_outbox (
    event_id        UUID PRIMARY KEY,
    event_type      VARCHAR(128) NOT NULL,
    tenant_id       UUID NOT NULL,
    partition_key   VARCHAR(255) NOT NULL,

    -- The fully rendered envelope, so what is delivered is what was decided
    -- even if the envelope struct changes shape in a later release.
    payload         JSONB NOT NULL,

    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    published_at    TIMESTAMP WITH TIME ZONE,
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_error      TEXT,

    CONSTRAINT event_outbox_attempts_nonneg CHECK (attempts >= 0)
);

CREATE INDEX idx_event_outbox_pending
    ON event_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

ALTER TABLE event_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_outbox FORCE ROW LEVEL SECURITY;

-- Two ways in, and the second is deliberately narrow and greppable. Normal
-- writes are tenant-scoped like every other table. The RELAY drains every
-- tenant's events from one process and so cannot set a single app.tenant_id;
-- it sets app.outbox_relay instead. That is a named capability rather than an
-- implicit superuser bypass: it appears in exactly one place in the code
-- (outbox.Relay), grants access to no other table, and a grep finds every
-- caller. Same shape as identity-context-svc's event_outbox policy.
CREATE POLICY tenant_isolation_policy ON event_outbox
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID
        OR current_setting('app.outbox_relay', true) = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID
        OR current_setting('app.outbox_relay', true) = 'true'
    );

-- ---------------------------------------------------------------------------
-- 6. FORCE row-level security on the pre-existing tables
-- ---------------------------------------------------------------------------
--
-- backend-completion-tracker.md Priority 3 row 56. ENABLE exempts the table
-- owner from its own policies; FORCE does not. The runtime role zoiko_app is
-- deliberately not the owner so this is defense-in-depth against a future
-- owner-role connection, not a fix to a live leak.
--
-- Also adds the WITH CHECK half the 000002 policies omit. USING alone governs
-- which rows are VISIBLE; without WITH CHECK an UPDATE may move a visible row
-- to another tenant's id. Postgres defaults WITH CHECK to the USING expression
-- when omitted, so this is explicit rather than new behaviour — worth being
-- explicit about, because the default is not obvious from reading the policy.

ALTER TABLE tenants                          FORCE ROW LEVEL SECURITY;
ALTER TABLE data_residency_policies          FORCE ROW LEVEL SECURITY;
ALTER TABLE legal_entities                   FORCE ROW LEVEL SECURITY;
ALTER TABLE entity_hierarchies               FORCE ROW LEVEL SECURITY;
ALTER TABLE entity_jurisdiction_assignments  FORCE ROW LEVEL SECURITY;
ALTER TABLE tax_identity_bundles             FORCE ROW LEVEL SECURITY;
ALTER TABLE workspaces                       FORCE ROW LEVEL SECURITY;

DROP POLICY tenant_isolation_policy ON tenants;
-- tenants carries a SECOND named capability, for the same reason event_outbox
-- does: one read must happen before a tenant is known.
--
-- ResolveTenantByHost answers "which tenant owns this hostname, and may it
-- transact" -- the question an ingress layer asks BEFORE it has a session. It
-- reads tenant_host_bindings (no RLS, no tenant data) joined to tenants, and
-- that join is subject to this policy, so without an escape hatch the join
-- returns nothing and every hostname looks unbound.
--
-- app.tenant_resolve is deliberately narrow and greppable, exactly like
-- app.outbox_relay: it appears in ONE place in the code (PgStore.
-- ResolveTenantByHost), it is transaction-local, and it grants visibility on
-- THIS TABLE ONLY -- a caller holding it can read no legal entity, no
-- workspace, no residency policy and no profile version.
--
-- What it exposes is a tenant's code, status and lifecycle state to something
-- that already knows the tenant's hostname. That is the answer the caller came
-- for; withholding it would only force a second round trip it cannot make.
CREATE POLICY tenant_isolation_policy ON tenants
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID
        OR current_setting('app.tenant_resolve', true) = 'true'
    )
    WITH CHECK (
        -- Deliberately NOT extended: the resolve capability is read-only. A
        -- caller holding it can see a tenant row and cannot write one.
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID
    );

DROP POLICY tenant_isolation_policy ON data_residency_policies;
CREATE POLICY tenant_isolation_policy ON data_residency_policies
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY tenant_isolation_policy ON legal_entities;
CREATE POLICY tenant_isolation_policy ON legal_entities
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY tenant_isolation_policy ON entity_hierarchies;
CREATE POLICY tenant_isolation_policy ON entity_hierarchies
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY tenant_isolation_policy ON entity_jurisdiction_assignments;
CREATE POLICY tenant_isolation_policy ON entity_jurisdiction_assignments
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY tenant_isolation_policy ON tax_identity_bundles;
CREATE POLICY tenant_isolation_policy ON tax_identity_bundles
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY tenant_isolation_policy ON workspaces;
CREATE POLICY tenant_isolation_policy ON workspaces
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- ---------------------------------------------------------------------------
-- 7. Backfill: one profile version per existing legal entity
-- ---------------------------------------------------------------------------
--
-- Without this an entity created before this migration has no profile history
-- at all, and GetLegalEntityAsOf would return "not found" for an entity that
-- plainly exists. effective_from is the entity's own created_at, which is the
-- earliest business date this platform can honestly claim the profile was in
-- force from — not NOW(), which would assert the entity had no identity until
-- the migration ran.

INSERT INTO legal_entity_profile_versions (
    profile_version_id, tenant_id, legal_entity_id, version_number,
    legal_name, trading_name, registration_number, default_currency_code,
    incorporation_jurisdiction_id,
    effective_from, effective_to, recorded_at,
    change_reason, created_by_principal_id
)
SELECT
    gen_random_uuid(), e.tenant_id, e.legal_entity_id, 1,
    e.legal_name, e.trading_name, e.registration_number, e.default_currency_code,
    e.primary_jurisdiction_id,
    e.created_at, NULL, e.created_at,
    'INITIAL_BACKFILL', e.created_by_principal_id
FROM legal_entities e;
