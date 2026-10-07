-- Migration: 000007_gov01_completion.up.sql
--
-- Closes the GOV-01 contract gaps identified against
-- ZoikoSuite_Governance_Control_Plane_Detailed_Service_Specifications section 4:
--
--   * event_outbox            -- the transactional outbox 000005 deliberately
--                               deferred. Events are now written in the SAME
--                               transaction as the fact they attest, and a
--                               relay drains them to Kafka.
--   * support_contexts        -- AttachSupportContext, the privileged command
--                               that had no implementation at all.
--   * tenant_ingress_bindings -- the "TenantRoutingHint cache" GOV-01's
--                               authoritative-ownership row names. Without it
--                               negative-path #2 ("unknown hostname cannot
--                               fall back to another tenant") was not merely
--                               untested but untestable: no hostname reached
--                               tenant resolution at all.
--   * legal_hold_projection   -- a READ-ONLY projection of GOV-10's holds, so
--                               disposition can be blocked. GOV-01 must never
--                               own the matter itself (authority matrix s3),
--                               so this is populated from events and carries
--                               no release capability.
--   * session_contexts        -- ingress_source, environment, evidence_id,
--                               support_context_id and the retention columns
--                               the TenantContextDecision entity requires.
--
-- ID columns are VARCHAR throughout, matching 000001: these are ULIDs, not
-- Postgres UUID literals.
--
-- Migrations are run via golang-migrate CLI in CI/CD. Do NOT auto-run on
-- service startup.

-- ---------------------------------------------------------------------------
-- 1. Transactional outbox
-- ---------------------------------------------------------------------------
--
-- The row is written inside the caller's transaction. If the business write
-- rolls back, so does the event; if it commits, the event is durable and the
-- relay will deliver it at least once. That is the whole point: before this,
-- every publish was a fire-and-forget goroutine holding a detached context,
-- so a broker blip or a SIGTERM lost the evidence for a resolution that had
-- already succeeded -- and the service reported success either way.

CREATE TABLE event_outbox (
    event_id        VARCHAR(255) PRIMARY KEY,
    event_type      VARCHAR(200) NOT NULL,

    -- Empty string, not NULL, for events that genuinely resolved no tenant --
    -- a failed resolution may never have got that far. NULL would be
    -- indistinguishable from "we forgot to set it", and the relay's RLS
    -- predicate would treat the two differently.
    tenant_id       VARCHAR(255) NOT NULL DEFAULT '',

    -- Kafka message key. Ordering within a partition is per key, so this is
    -- the session/principal the event is about, not the event id.
    partition_key   VARCHAR(255) NOT NULL,

    -- The complete platform event envelope, exactly as it will be written to
    -- the topic. Stored rendered rather than reassembled at publish time so
    -- what is delivered is what was decided, even if the envelope struct
    -- changes shape in a later release.
    payload         JSONB NOT NULL,

    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    published_at    TIMESTAMP WITH TIME ZONE,
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_error      TEXT,

    CONSTRAINT event_outbox_attempts_nonneg CHECK (attempts >= 0)
);

-- The relay's only query: unpublished, due, oldest first.
CREATE INDEX idx_event_outbox_pending
    ON event_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

-- Published rows are kept for the evidence window and swept by the retention
-- worker, so this index stays small.
CREATE INDEX idx_event_outbox_published
    ON event_outbox (published_at)
    WHERE published_at IS NOT NULL;

ALTER TABLE event_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_outbox FORCE ROW LEVEL SECURITY;

-- Two ways in, and the second is deliberately narrow and greppable.
--
-- Normal writes are tenant-scoped exactly like every other table here. The
-- RELAY, however, drains every tenant's events from one process and cannot
-- set a single app.tenant_id -- so it sets app.outbox_relay instead. That is a
-- named capability rather than an implicit superuser bypass: it appears in
-- exactly one place in the codebase (outbox.Relay), it grants no access to any
-- other table, and a grep for it finds every caller.
CREATE POLICY tenant_isolation_policy ON event_outbox
    FOR ALL
    USING (
        tenant_id = current_setting('app.tenant_id', true)
        OR current_setting('app.outbox_relay', true) = 'true'
    )
    WITH CHECK (
        tenant_id = current_setting('app.tenant_id', true)
        OR current_setting('app.outbox_relay', true) = 'true'
    );

-- ---------------------------------------------------------------------------
-- 2. Support contexts  (AttachSupportContext)
-- ---------------------------------------------------------------------------
--
-- GOV-01 names AttachSupportContext as its one privileged command, and the
-- shared invariants are explicit that this is not a super-admin door:
-- "Emergency elevation is scoped, time-limited, independently approved, fully
-- evidenced and followed by reconciliation/review."
--
-- Every one of those five words is a column here, and each is NOT NULL:
--   scoped                -> tenant_id (+ optional subject_principal_id)
--   time-limited          -> expires_at, bounded in app by the max-TTL config
--   independently approved-> approver_principal_id, CHECKed against grantee
--   fully evidenced       -> evidence_id, reason_code, justification, ticket
--   reviewed              -> reviewed_at / reviewed_by, swept by the reconciler

CREATE TABLE support_contexts (
    support_context_id    VARCHAR(255) PRIMARY KEY,

    -- The tenant being supported. This is the scope the attached context
    -- grants visibility into, and it is never widened after the fact.
    tenant_id             VARCHAR(255) NOT NULL,

    -- The staff principal receiving the elevation. Lives in the SUPPORT
    -- tenant, not in tenant_id -- which is why there is no FK here: a foreign
    -- key to principals would force the support engineer to exist inside the
    -- customer tenant they are supporting.
    support_principal_id  VARCHAR(255) NOT NULL,

    -- Optional narrowing to a single principal's data.
    subject_principal_id  VARCHAR(255),

    reason_code           VARCHAR(100) NOT NULL,
    justification         TEXT         NOT NULL,
    ticket_ref            VARCHAR(255) NOT NULL,

    -- Maker-checker (GOV-12). The approver is a different human, enforced
    -- both here and by the SoD check in the command handler.
    approver_principal_id VARCHAR(255) NOT NULL,

    granted_at            TIMESTAMP WITH TIME ZONE NOT NULL,
    expires_at            TIMESTAMP WITH TIME ZONE NOT NULL,

    -- Append-only, same doctrine as session_contexts.invalidated_at.
    revoked_at            TIMESTAMP WITH TIME ZONE,
    revocation_reason     VARCHAR(100),

    -- Post-hoc reconciliation, per the "followed by review" clause.
    reviewed_at           TIMESTAMP WITH TIME ZONE,
    reviewed_by           VARCHAR(255),

    evidence_id           VARCHAR(255) NOT NULL,
    correlation_id        VARCHAR(255) NOT NULL,
    created_at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    -- A support context that never expires is a standing back door.
    CONSTRAINT support_contexts_window_check
        CHECK (expires_at > granted_at),

    -- Self-approval defeats maker-checker entirely. Refused in the schema as
    -- well as the handler, because this is the control that a future caller
    -- reaching the table directly would otherwise skip.
    CONSTRAINT support_contexts_independent_approval_check
        CHECK (approver_principal_id <> support_principal_id),

    CONSTRAINT support_contexts_revocation_pair_check
        CHECK ((revoked_at IS NULL) = (revocation_reason IS NULL)),

    CONSTRAINT support_contexts_review_pair_check
        CHECK ((reviewed_at IS NULL) = (reviewed_by IS NULL))
);

-- "is there a live support context for this staff member in this tenant"
-- -- the read on every support-scoped request.
CREATE INDEX idx_support_contexts_live
    ON support_contexts (support_principal_id, tenant_id, expires_at)
    WHERE revoked_at IS NULL;

-- "what has not yet been reviewed" -- the reconciliation sweep.
CREATE INDEX idx_support_contexts_unreviewed
    ON support_contexts (expires_at)
    WHERE reviewed_at IS NULL;

ALTER TABLE support_contexts ENABLE ROW LEVEL SECURITY;
ALTER TABLE support_contexts FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON support_contexts
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- ---------------------------------------------------------------------------
-- 3. Tenant ingress bindings  (the TenantRoutingHint cache)
-- ---------------------------------------------------------------------------
--
-- GOV-01's required source inputs include "hostname or canonical ingress
-- identifier", and negative path #2 requires that an unknown one cannot fall
-- back to another tenant. Nothing read ingress before, so the resolver took
-- the tenant solely from the token claim and there was no fallback to test.
--
-- This is a CACHE of a fact the tenant registry owns, not the master. The
-- authority matrix is explicit that GOV-01 must never own tenant master data,
-- so rows here are a local read model: they can CONTRADICT a token (which is
-- a refusal) but they can never GRANT a tenant the token did not claim.

CREATE TABLE tenant_ingress_bindings (
    -- Lowercased hostname or canonical ingress id. The primary key IS the
    -- lookup, so an unknown host misses rather than matching a default row.
    ingress_identifier VARCHAR(255) PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    environment        VARCHAR(50)  NOT NULL,
    active_flag        BOOLEAN      NOT NULL DEFAULT TRUE,
    source_version     VARCHAR(64)  NOT NULL DEFAULT '',
    refreshed_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    CONSTRAINT tenant_ingress_bindings_identifier_lowercase_check
        CHECK (ingress_identifier = LOWER(ingress_identifier)),

    CONSTRAINT tenant_ingress_bindings_environment_check
        CHECK (environment IN ('local', 'development', 'staging', 'production'))
);

CREATE INDEX idx_tenant_ingress_bindings_tenant
    ON tenant_ingress_bindings (tenant_id)
    WHERE active_flag;

-- NO row-level security, and that is deliberate rather than an omission.
--
-- This table is read BEFORE a tenant is resolved -- it is what resolves one.
-- A tenant-scoped policy would require app.tenant_id to already be set, which
-- is the answer the lookup exists to produce. It holds no tenant data beyond
-- the hostname-to-tenant mapping itself, which is public in practice: anyone
-- can observe which hostname serves which customer by connecting to it.

-- ---------------------------------------------------------------------------
-- 4. Legal-hold projection  (READ MODEL of GOV-10)
-- ---------------------------------------------------------------------------
--
-- Authority matrix section 3: GOV-01 must never own "legal-hold matter or
-- privacy request authority". This table therefore has no issue/release path
-- in the service -- it is written ONLY by the event consumer from GOV-10's
-- LegalHoldIssued / HoldScopeChanged / LegalHoldReleased events, and read
-- ONLY by the disposition worker to refuse a deletion.
--
-- A hold suspends disposition. Invariant 7 is explicit that it never creates
-- a new processing purpose, so nothing here widens visibility or access.

CREATE TABLE legal_hold_projection (
    hold_id          VARCHAR(255) PRIMARY KEY,
    tenant_id        VARCHAR(255) NOT NULL,
    matter_ref       VARCHAR(255) NOT NULL,

    -- NULL means the hold covers the whole tenant. A hold that named a
    -- principal it could not resolve must still block, so this is a plain
    -- column with no FK.
    principal_id     VARCHAR(255),

    issued_at        TIMESTAMP WITH TIME ZONE NOT NULL,
    released_at      TIMESTAMP WITH TIME ZONE,

    -- Last event that changed this row, so a replayed or out-of-order
    -- delivery cannot resurrect a released hold.
    last_event_id    VARCHAR(255) NOT NULL,
    last_event_at    TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_legal_hold_projection_active
    ON legal_hold_projection (tenant_id, principal_id)
    WHERE released_at IS NULL;

ALTER TABLE legal_hold_projection ENABLE ROW LEVEL SECURITY;
ALTER TABLE legal_hold_projection FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON legal_hold_projection
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- ---------------------------------------------------------------------------
-- 5. session_contexts -- the TenantContextDecision fields that were missing
-- ---------------------------------------------------------------------------
--
-- The cross-service data model (section 18) specifies TenantContextDecision as
-- "decision_id, subject, ingress source, tenant, entity, environment,
-- assurance, expires_at". Ingress source and environment had no column, so a
-- resolution could not be explained after the fact and negative path #2 had
-- nothing to assert against.

ALTER TABLE session_contexts
    -- The canonical ingress identifier the request arrived on. 'UNKNOWN' for
    -- a call that presented none -- recorded as a value rather than left NULL,
    -- for the same reason risk_signal_source records UNAVAILABLE.
    ADD COLUMN ingress_source VARCHAR(255) NOT NULL DEFAULT 'UNKNOWN',

    ADD COLUMN environment    VARCHAR(50)  NOT NULL DEFAULT 'local',

    -- The evidence object this decision produced. Returned on the wire so a
    -- caller can cite the decision that granted it (section 16: "evidence_id
    -- returned for material governance decision/transition").
    ADD COLUMN evidence_id    VARCHAR(255) NOT NULL DEFAULT '',

    -- Set when the session was issued under a support elevation, so every
    -- action taken during support is attributable to the grant that allowed
    -- it. NULL is the overwhelmingly common case: ordinary sessions.
    ADD COLUMN support_context_id VARCHAR(255),

    -- Retention (GOV-09). The class decides the period; the due date is
    -- computed at write time so the disposition sweep is an index scan rather
    -- than a policy evaluation per row.
    ADD COLUMN retention_class    VARCHAR(100) NOT NULL DEFAULT 'IDENTITY_SESSION_EVIDENCE',
    ADD COLUMN disposition_due_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN disposed_at        TIMESTAMP WITH TIME ZONE;

ALTER TABLE session_contexts
    ADD CONSTRAINT session_contexts_environment_check
        CHECK (environment IN ('local', 'development', 'staging', 'production'));

ALTER TABLE session_contexts
    ADD CONSTRAINT support_context_fk
        FOREIGN KEY (support_context_id) REFERENCES support_contexts (support_context_id);

-- "what is due for disposition" -- partial, because the overwhelming majority
-- of rows are not yet due and none that are disposed ever become due again.
CREATE INDEX idx_session_contexts_disposition_due
    ON session_contexts (disposition_due_at)
    WHERE disposed_at IS NULL AND disposition_due_at IS NOT NULL;

-- "which sessions ran under this support grant" -- the reconciliation read.
CREATE INDEX idx_session_contexts_support
    ON session_contexts (support_context_id)
    WHERE support_context_id IS NOT NULL;

-- "what did this tenant's context resolution look like at time T" -- the
-- as-of reconstruction query (DoD gate 6).
CREATE INDEX idx_session_contexts_tenant_issued
    ON session_contexts (tenant_id, issued_at DESC);

-- ---------------------------------------------------------------------------
-- 6. access_decision_log -- same retention treatment
-- ---------------------------------------------------------------------------

ALTER TABLE access_decision_log
    ADD COLUMN retention_class    VARCHAR(100) NOT NULL DEFAULT 'IDENTITY_ACCESS_EVIDENCE',
    ADD COLUMN disposition_due_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN disposed_at        TIMESTAMP WITH TIME ZONE;

CREATE INDEX idx_access_decision_log_disposition_due
    ON access_decision_log (disposition_due_at)
    WHERE disposed_at IS NULL AND disposition_due_at IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 7. The retention sweep's cross-tenant capability
-- ---------------------------------------------------------------------------
--
-- A retention obligation that applies only to the tenants somebody remembered
-- to configure is not an obligation. The sweep therefore has to enumerate
-- tenants, which no tenant-scoped connection can do.
--
-- Rather than run the sweep as a superuser -- which would grant it every
-- table, every column and every operation -- it gets a NAMED capability with
-- the same shape as the relay's, and the policy grants it exactly two things:
-- the ability to SEE rows across tenants on the two evidence tables it
-- disposes. It still cannot read principals, credentials, delegations or
-- support contexts, and it appears in exactly one place in the codebase.
--
-- The policies are replaced rather than added to, because a second permissive
-- policy on the same table ORs with the first and the combined effect is far
-- harder to reason about than one predicate that states both cases.

DROP POLICY tenant_isolation_policy ON session_contexts;
CREATE POLICY tenant_isolation_policy ON session_contexts
    FOR ALL
    USING (
        tenant_id = current_setting('app.tenant_id', true)
        OR current_setting('app.retention_sweep', true) = 'true'
    )
    WITH CHECK (
        tenant_id = current_setting('app.tenant_id', true)
        OR current_setting('app.retention_sweep', true) = 'true'
    );

DROP POLICY tenant_isolation_policy ON access_decision_log;
CREATE POLICY tenant_isolation_policy ON access_decision_log
    FOR ALL
    USING (
        tenant_id = current_setting('app.tenant_id', true)
        OR current_setting('app.retention_sweep', true) = 'true'
    )
    WITH CHECK (
        tenant_id = current_setting('app.tenant_id', true)
        OR current_setting('app.retention_sweep', true) = 'true'
    );
