-- 20260818001900_secret_vault_integration_svc.sql
-- secret-vault-integration-svc → schema `secret_vault_integration`
--
-- Squashed end state of 000001_initial_schema and
-- 000002_add_data_classification. Four tables: secret_policies,
-- secret_policy_versions, secret_leases, secret_access_audit_log.
--
-- ── This service stores no secret VALUES ─────────────────────────────────────
-- Only policy metadata, lease metadata, and audit records referencing an opaque
-- secret_path. The real material lives behind the VaultBackend interface, not
-- in this database. That is worth restating because it is the reason the tables
-- below can exist in a general-purpose Postgres at all.
--
-- ── No `authenticated` access to ANY table in this schema ────────────────────
-- Every other service in this migration set grants console sessions a
-- tenant-scoped SELECT. This one grants them nothing, and the schema is not
-- granted USAGE to them either.
--
-- The reason is that a secret PATH is itself sensitive: the set of paths, who
-- brokered which, and when, is a map of the platform's credentials and of who
-- holds them. secret_policies carries data_classification RESTRICTED by
-- default. Exposing these tables through PostgREST would put that map one JWT
-- away from any logged-in console user, which no route on this service does
-- today. Reads go through the service, which authorises them.

CREATE SCHEMA IF NOT EXISTS secret_vault_integration;

COMMENT ON SCHEMA secret_vault_integration IS
    'secret-vault-integration-svc. Vault policy, lease and access-audit metadata. Never secret values. Deliberately not exposed to the authenticated role.';

GRANT USAGE ON SCHEMA secret_vault_integration TO zoiko_backend;

-- ── secret_policies ──────────────────────────────────────────────────────────
-- No tenant dimension: a vault path is a platform-wide address, and scoping
-- happens on the versions below.

CREATE TABLE secret_vault_integration.secret_policies (
    secret_policy_id        UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Data-driven, never a code switch/case.
    secret_class            VARCHAR(64) NOT NULL,

    -- The opaque reference/path in the underlying vault backend — never the
    -- secret value. This is the natural unique key ON ITS OWN: a vault path is
    -- already a unique address by construction, so the key is secret_path and
    -- not (secret_class, secret_path).
    secret_path             TEXT        NOT NULL,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT        NOT NULL DEFAULT app.current_principal_id(),

    data_classification     VARCHAR(32) NOT NULL DEFAULT 'RESTRICTED',

    CONSTRAINT secret_policies_path_present
        CHECK (btrim(secret_path) <> '')
);

CREATE UNIQUE INDEX idx_secret_policies_path_unique
    ON secret_vault_integration.secret_policies (secret_path);
CREATE INDEX idx_secret_policies_class
    ON secret_vault_integration.secret_policies (secret_class);

-- ── secret_policy_versions ───────────────────────────────────────────────────

CREATE TABLE secret_vault_integration.secret_policy_versions (
    secret_policy_version_id   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    secret_policy_id           UUID        NOT NULL
        REFERENCES secret_vault_integration.secret_policies(secret_policy_id),

    -- NULL tenant_id = global policy.
    tenant_id                  UUID,
    -- NULL legal_entity_id = applies to the whole tenant, or globally.
    legal_entity_id            UUID,

    -- Workload / service / principal identifiers permitted to broker this
    -- secret in this scope. JSONB array — data, not schema.
    allowed_workload_ids       JSONB       NOT NULL DEFAULT '[]',

    max_lease_duration_seconds INTEGER     NOT NULL,

    effective_from             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_to               TIMESTAMPTZ,

    -- DRAFT | ACTIVE | SUPERSEDED | RETIRED
    version_status             VARCHAR(32) NOT NULL DEFAULT 'DRAFT',

    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id    TEXT        NOT NULL DEFAULT app.current_principal_id(),

    CONSTRAINT secret_policy_versions_status_known
        CHECK (version_status IN ('DRAFT', 'ACTIVE', 'SUPERSEDED', 'RETIRED')),

    CONSTRAINT secret_policy_versions_lease_duration_positive
        CHECK (max_lease_duration_seconds > 0),

    CONSTRAINT secret_policy_versions_period_ordered
        CHECK (effective_to IS NULL OR effective_to > effective_from),

    -- allowed_workload_ids is consulted to decide who may broker this secret.
    -- A non-array there is not a permissive list, it is an unreadable one.
    CONSTRAINT secret_policy_versions_workloads_is_array
        CHECK (jsonb_typeof(allowed_workload_ids) = 'array')
);

-- Idempotent creation key for a version within a policy + scope + effective_from.
CREATE UNIQUE INDEX idx_secret_policy_versions_dedup
    ON secret_vault_integration.secret_policy_versions (
        secret_policy_id,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
        COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID),
        effective_from
    );

CREATE INDEX idx_secret_policy_versions_scope
    ON secret_vault_integration.secret_policy_versions
       (secret_policy_id, tenant_id, legal_entity_id);

-- At most one ACTIVE version per scope. COALESCE for the same reason as
-- everywhere else: Postgres treats NULLs as distinct in a unique index, so
-- without it "one active global policy" would not hold at all.
CREATE UNIQUE INDEX idx_secret_policy_versions_one_active_per_scope
    ON secret_vault_integration.secret_policy_versions (
        secret_policy_id,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
        COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID)
    )
    WHERE version_status = 'ACTIVE';

CREATE INDEX idx_secret_policy_versions_history
    ON secret_vault_integration.secret_policy_versions (secret_policy_id, effective_from DESC);

-- ── secret_leases ────────────────────────────────────────────────────────────
-- Grants only. A denial never becomes a lease — denials exist solely in
-- secret_access_audit_log. Effective-dated and revocable, never hard-deleted.

CREATE TABLE secret_vault_integration.secret_leases (
    lease_id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Caller-supplied idempotency key: a retried broker request with the same
    -- request_id must never mint a second lease.
    request_id                TEXT        NOT NULL,

    secret_policy_version_id  UUID        NOT NULL
        REFERENCES secret_vault_integration.secret_policy_versions(secret_policy_version_id),

    -- Denormalised from the resolved policy at grant time, so this row is
    -- self-contained evidence even after the policy is superseded.
    secret_class              VARCHAR(64) NOT NULL,
    secret_path               TEXT        NOT NULL,

    requested_by_principal_id TEXT        NOT NULL,
    tenant_id                 UUID,
    legal_entity_id           UUID,

    -- GRANTED | EXPIRED | REVOKED. EXPIRED is a COMPUTED read
    -- (status = 'GRANTED' AND expires_at < NOW()), never a background job
    -- flipping rows — so a lease that has run out is expired the instant it
    -- runs out, with no window in which a sweep has not caught up yet.
    status                    VARCHAR(32) NOT NULL DEFAULT 'GRANTED',

    granted_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at                TIMESTAMPTZ NOT NULL,
    revoked_at                TIMESTAMPTZ,

    correlation_id            TEXT,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT secret_leases_status_known
        CHECK (status IN ('GRANTED', 'EXPIRED', 'REVOKED')),

    -- A lease that expires before it is granted is not a lease.
    CONSTRAINT secret_leases_expiry_after_grant
        CHECK (expires_at > granted_at),

    -- A revoked lease must say when. Written as an equivalence so a revoked_at
    -- cannot linger on a row rewritten back to GRANTED.
    CONSTRAINT secret_leases_revoked_has_timestamp
        CHECK ((status = 'REVOKED') = (revoked_at IS NOT NULL))
);

CREATE UNIQUE INDEX idx_secret_leases_request_id_unique
    ON secret_vault_integration.secret_leases (request_id);
CREATE INDEX idx_secret_leases_principal
    ON secret_vault_integration.secret_leases (requested_by_principal_id);
CREATE INDEX idx_secret_leases_secret_path
    ON secret_vault_integration.secret_leases (secret_path);
CREATE INDEX idx_secret_leases_status
    ON secret_vault_integration.secret_leases (status) WHERE status = 'GRANTED';
CREATE INDEX idx_secret_leases_granted_at
    ON secret_vault_integration.secret_leases (granted_at);

-- ── secret_access_audit_log ──────────────────────────────────────────────────
-- Append-only, same guarantees as governance_decisions: no UPDATE, no DELETE,
-- ever. This is where a DENIED decision lives, so it is the only record that a
-- refusal happened at all.

CREATE TABLE secret_vault_integration.secret_access_audit_log (
    audit_log_id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- REQUESTED | GRANTED | DENIED | REVOKED | ROTATED
    event_type                VARCHAR(32) NOT NULL,

    secret_class              VARCHAR(64) NOT NULL,
    secret_path               TEXT        NOT NULL,

    requested_by_principal_id TEXT        NOT NULL,
    tenant_id                 UUID,
    legal_entity_id           UUID,

    -- NULL for REQUESTED and DENIED — nothing was granted to reference. Set for
    -- every lease revoked by a ROTATED event too.
    lease_id                  UUID
        REFERENCES secret_vault_integration.secret_leases(lease_id),

    -- NULL for DENIED when no policy existed at all for that path or scope.
    secret_policy_version_id  UUID
        REFERENCES secret_vault_integration.secret_policy_versions(secret_policy_version_id),

    -- Populated and deduped for ROTATED entries only: rotation needs its own
    -- idempotency path, distinct from secret_leases.request_id, because a
    -- rotate call creates no lease row.
    request_id                TEXT,

    outcome_detail            TEXT,
    correlation_id            TEXT,
    recorded_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT secret_access_audit_log_event_type_known
        CHECK (event_type IN ('REQUESTED', 'GRANTED', 'DENIED', 'REVOKED', 'ROTATED')),

    -- A GRANTED audit entry that references no lease is a claim that access was
    -- given with nothing recording what was given.
    CONSTRAINT secret_access_audit_log_granted_has_lease
        CHECK (event_type <> 'GRANTED' OR lease_id IS NOT NULL)
);

CREATE UNIQUE INDEX idx_secret_access_audit_log_rotation_request_id
    ON secret_vault_integration.secret_access_audit_log (request_id)
    WHERE event_type = 'ROTATED' AND request_id IS NOT NULL;

CREATE INDEX idx_secret_access_audit_log_principal
    ON secret_vault_integration.secret_access_audit_log (requested_by_principal_id);
CREATE INDEX idx_secret_access_audit_log_secret_path
    ON secret_vault_integration.secret_access_audit_log (secret_path);
CREATE INDEX idx_secret_access_audit_log_event_type
    ON secret_vault_integration.secret_access_audit_log (event_type);
CREATE INDEX idx_secret_access_audit_log_recorded_at
    ON secret_vault_integration.secret_access_audit_log (recorded_at);

CREATE TRIGGER secret_access_audit_log_immutable
    BEFORE UPDATE OR DELETE ON secret_vault_integration.secret_access_audit_log
    FOR EACH ROW EXECUTE FUNCTION app.reject_mutation();

-- ── Row-level security ───────────────────────────────────────────────────────
-- secret_policies has no tenant column at all, so its policy is a plain
-- backend-only gate. The other three carry a NULLABLE tenant_id where NULL
-- means global, so they take the asymmetric shape used by
-- configuration-feature-flag: globals are readable by everyone, but writable
-- only by a connection that has installed no tenant — a tenant connection can
-- never mint a global.

ALTER TABLE secret_vault_integration.secret_policies          ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_vault_integration.secret_policies          FORCE  ROW LEVEL SECURITY;
ALTER TABLE secret_vault_integration.secret_policy_versions   ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_vault_integration.secret_policy_versions   FORCE  ROW LEVEL SECURITY;
ALTER TABLE secret_vault_integration.secret_leases            ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_vault_integration.secret_leases            FORCE  ROW LEVEL SECURITY;
ALTER TABLE secret_vault_integration.secret_access_audit_log  ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_vault_integration.secret_access_audit_log  FORCE  ROW LEVEL SECURITY;

CREATE POLICY backend_all ON secret_vault_integration.secret_policies
    FOR ALL TO zoiko_backend USING (true) WITH CHECK (true);

CREATE POLICY tenant_isolation ON secret_vault_integration.secret_policy_versions
    FOR ALL
    TO zoiko_backend
    USING (tenant_id IS NULL OR tenant_id::text = app.current_tenant_id())
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id::text = app.current_tenant_id())
        OR (tenant_id IS NULL AND app.current_tenant_id() IS NULL)
    );

CREATE POLICY tenant_isolation ON secret_vault_integration.secret_leases
    FOR ALL
    TO zoiko_backend
    USING (tenant_id IS NULL OR tenant_id::text = app.current_tenant_id())
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id::text = app.current_tenant_id())
        OR (tenant_id IS NULL AND app.current_tenant_id() IS NULL)
    );

CREATE POLICY tenant_isolation ON secret_vault_integration.secret_access_audit_log
    FOR ALL
    TO zoiko_backend
    USING (tenant_id IS NULL OR tenant_id::text = app.current_tenant_id())
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id::text = app.current_tenant_id())
        OR (tenant_id IS NULL AND app.current_tenant_id() IS NULL)
    );

-- ── Grants ───────────────────────────────────────────────────────────────────
-- No grants to `authenticated` anywhere in this schema — see the header.

GRANT SELECT, INSERT, UPDATE ON secret_vault_integration.secret_policies        TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE ON secret_vault_integration.secret_policy_versions TO zoiko_backend;

-- Leases transition (GRANTED → REVOKED), so UPDATE. Never deleted.
GRANT SELECT, INSERT, UPDATE ON secret_vault_integration.secret_leases          TO zoiko_backend;

-- The audit log is append-only.
GRANT SELECT, INSERT         ON secret_vault_integration.secret_access_audit_log TO zoiko_backend;
