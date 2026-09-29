-- access-control-svc: a transactional outbox for the catalogue events, and the
-- uniqueness rule that stops two local bundles pointing at one remote grant.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- 1. THE OUTBOX
--
-- Until now the three catalogue events were written straight to Kafka from the
-- handler, after the store transaction had already committed, with the error
-- logged and discarded. On a service whose events are advisory that is a
-- tolerable trade. On this one it is not.
--
-- identity-context-svc consumes role.updated and revokes the sessions of every
-- principal holding the role, because a role's permission bundles are FROZEN
-- INTO THE SESSION ENVELOPE at resolve time. A broker hiccup during a
-- retirement therefore left: the register correctly showing RETIRED,
-- authorization-svc correctly refusing new authorize calls, the operator
-- correctly told the retirement succeeded — and every session already holding
-- that role still carrying every action it used to grant, until it expired.
-- Nothing anywhere reported a fault.
--
-- Writing the event in the SAME transaction as the state change makes the two
-- atomic: a definition that changed always has its event, and an event that
-- exists always has its change. Delivery then becomes a separate, retryable
-- problem that internal/outbox owns and that the metrics make visible.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- 2. BUNDLE CODE UNIQUENESS
--
-- authorization-svc identifies a permission bundle by (role_id, bundle_code),
-- and its attach endpoint is an upsert-REPLACE on that pair. This table had no
-- matching constraint, so two local bundles could carry the same code on the
-- same role. The consequences were both silent:
--
--   * Creating the second one REPLACED the first one's permitted_actions in
--     authorization-svc. Both rows stayed ACTIVE here, listing different
--     actions, while only the later set was enforced.
--   * Detaching either one retired the single remote bundle they shared, so
--     the other went on being displayed as ACTIVE while granting nothing.
--
-- A register that disagrees with the enforcement it describes is worse than no
-- register. The index below makes the local key match the remote one.

-- ── 1. Outbox ────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS event_outbox (
    outbox_id       BIGSERIAL PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    event_type      VARCHAR(100) NOT NULL,
    -- The Kafka partition key: the role or bundle id the event is about. Kept
    -- as text rather than UUID because the two aggregates have different id
    -- columns and this table does not need to know which one it is holding.
    aggregate_key   VARCHAR(255) NOT NULL,
    -- The fully-built envelope, exactly as it will be written to Kafka. Built
    -- at write time rather than at publish time on purpose: the envelope
    -- carries the actor and the correlation id of the request that caused the
    -- change, and the relay runs long after that request context is gone.
    payload         JSONB NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    published_at    TIMESTAMP WITH TIME ZONE,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    CONSTRAINT event_outbox_event_known
        CHECK (event_type IN ('role.created', 'role.updated', 'permission.bundle.updated'))
);

-- The relay's claim query, and the only index it needs: unpublished rows,
-- oldest first. Partial, so the index stays the size of the BACKLOG rather than
-- the size of the history — on a healthy service that is a handful of rows
-- however many events have been delivered.
CREATE INDEX IF NOT EXISTS idx_event_outbox_unpublished
    ON event_outbox (created_at, outbox_id) WHERE published_at IS NULL;

ALTER TABLE event_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_outbox FORCE ROW LEVEL SECURITY;

-- Tenant isolation, with one deliberate escape hatch.
--
-- Every request-path write installs app.tenant_id and is confined to it. The
-- relay is not a request: it drains every tenant's backlog from one background
-- loop, so it installs app.outbox_relay instead and is admitted by the second
-- disjunct. Rather than letting it run unscoped — which under FORCE ROW LEVEL
-- SECURITY would simply see nothing, and would present as a relay that never
-- publishes anything — it names itself, so the policy admits it by an explicit,
-- auditable disjunct instead of by the absence of a control.
--
-- NULLIF is not decoration. Postgres keeps a custom GUC in the SESSION after a
-- transaction-local SET is reset, with '' as its value, so on any pooled
-- connection that has already served one request these settings read as the
-- empty string rather than NULL.
CREATE POLICY outbox_tenant_isolation ON event_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );

-- ── 2. One bundle_code per role ──────────────────────────────────────────────
--
-- Scoped by tenant_id as well, so the index is usable under RLS and two tenants
-- may of course each define their own PO_FULL.
--
-- Not NOT VALID: a unique index cannot be deferred that way, and it is created
-- unconditionally because the constraint must hold from now on. If a deployment
-- already holds duplicates this migration fails loudly, which is the correct
-- outcome — the duplicates are precisely the rows whose enforcement disagrees
-- with the register, and they need a human to decide which one survives. The
-- query that finds them:
--
--   SELECT tenant_id, role_definition_id, bundle_code, count(*)
--   FROM permission_bundle_defs GROUP BY 1,2,3 HAVING count(*) > 1;
CREATE UNIQUE INDEX IF NOT EXISTS idx_permission_bundle_defs_role_code
    ON permission_bundle_defs (tenant_id, role_definition_id, bundle_code);

-- ── 3. Indexes the catalogue reads actually use ──────────────────────────────
--
-- ListRoles filters on status and scope_type and orders by created_at; the only
-- index on this table was the UNIQUE (tenant_id, role_code) the writes need.
-- At five rows that is irrelevant and at fifty thousand it is a sequential scan
-- behind every page of the console's role catalogue.
CREATE INDEX IF NOT EXISTS idx_role_definitions_tenant_status
    ON role_definitions (tenant_id, status, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_permission_bundle_defs_tenant_active
    ON permission_bundle_defs (tenant_id, active_flag, created_at DESC);
