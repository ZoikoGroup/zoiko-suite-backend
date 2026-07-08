-- 000001_initial_schema.up.sql
-- Secret Vault Integration Service — initial schema
--
-- Owns four objects (03-microservices.md §9.5): vault integration policy
-- (secret_policies + secret_policy_versions), secret access brokering
-- (the POST /v1/secrets/broker operation, no dedicated table), secret
-- lease metadata (secret_leases), and access audit references
-- (secret_access_audit_log). See context.md §7.1 for the full design
-- record, including two corrections found on review (secret_path as the
-- sole natural key, and splitting leases from audit into separate
-- tables).
--
-- This service never stores an actual secret value here — only policy
-- metadata, lease metadata, and audit records referencing an opaque
-- secret_path. The real secret material lives behind the VaultBackend
-- interface (internal/vault), not in this database.

-- ── secret_policies ──────────────────────────────────────────────────────────

CREATE TABLE secret_policies (
    secret_policy_id        UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Data-driven, never a code switch/case (context.md §2's 8-value list).
    secret_class            VARCHAR(64) NOT NULL,

    -- The opaque reference/path in the underlying vault backend — never
    -- the secret value itself. This is the table's natural unique key on
    -- its own (a vault path is already a unique address by construction),
    -- not (secret_class, secret_path) — corrected during design review,
    -- see context.md §7.1.
    secret_path             TEXT        NOT NULL,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT        NOT NULL
);

CREATE UNIQUE INDEX idx_secret_policies_path_unique ON secret_policies (secret_path);
CREATE INDEX idx_secret_policies_class ON secret_policies (secret_class);

-- ── secret_policy_versions ───────────────────────────────────────────────────

CREATE TABLE secret_policy_versions (
    secret_policy_version_id UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    secret_policy_id         UUID        NOT NULL REFERENCES secret_policies(secret_policy_id),

    -- NULL tenant_id = global policy.
    tenant_id                UUID,
    -- NULL legal_entity_id = applies to the whole tenant (or globally).
    legal_entity_id          UUID,

    -- Workload/service/principal identifiers permitted to broker this
    -- secret in this scope. JSONB array, data not schema.
    allowed_workload_ids     JSONB       NOT NULL DEFAULT '[]',

    max_lease_duration_seconds INTEGER   NOT NULL,

    effective_from           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_to             TIMESTAMPTZ,

    -- DRAFT | ACTIVE | SUPERSEDED | RETIRED — VARCHAR, not enum.
    version_status           VARCHAR(32) NOT NULL DEFAULT 'DRAFT',

    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT        NOT NULL
);

-- Idempotent creation key for a version within a policy+scope+effective_from.
CREATE UNIQUE INDEX idx_secret_policy_versions_dedup ON secret_policy_versions (
    secret_policy_id,
    COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
    COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID),
    effective_from
);

CREATE INDEX idx_secret_policy_versions_scope
    ON secret_policy_versions (secret_policy_id, tenant_id, legal_entity_id);

-- At most one ACTIVE version per (secret_policy_id, tenant_id,
-- legal_entity_id) scope — enforced the same way as policy-svc's
-- idx_policy_versions_one_active_per_scope.
CREATE UNIQUE INDEX idx_secret_policy_versions_one_active_per_scope
    ON secret_policy_versions (
        secret_policy_id,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
        COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID)
    )
    WHERE version_status = 'ACTIVE';

CREATE INDEX idx_secret_policy_versions_history
    ON secret_policy_versions (secret_policy_id, effective_from DESC);

-- ── secret_leases ────────────────────────────────────────────────────────────
-- Grants only — denials never become leases (they only ever exist in
-- secret_access_audit_log below). Effective-dated and revocable, "same
-- doctrine as DelegatedAuthority elsewhere in the platform — no
-- hard-delete, ever" (direct instruction, context.md §7.1).

CREATE TABLE secret_leases (
    lease_id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Caller-supplied idempotency key — a retried broker request with the
    -- same request_id must never mint a second lease.
    request_id                TEXT        NOT NULL,

    secret_policy_version_id  UUID        NOT NULL REFERENCES secret_policy_versions(secret_policy_version_id),

    -- Denormalized from the resolved policy at grant time, so this row
    -- is self-contained evidence even if the policy is later superseded.
    secret_class              VARCHAR(64) NOT NULL,
    secret_path               TEXT        NOT NULL,

    requested_by_principal_id TEXT        NOT NULL,
    tenant_id                 UUID,
    legal_entity_id           UUID,

    -- GRANTED | EXPIRED | REVOKED — VARCHAR, not enum. EXPIRED is a
    -- computed read (status = 'GRANTED' AND expires_at < NOW()), never a
    -- background job flipping rows.
    status                    VARCHAR(32) NOT NULL DEFAULT 'GRANTED',

    granted_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at                TIMESTAMPTZ NOT NULL,
    revoked_at                TIMESTAMPTZ,

    correlation_id            TEXT,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_secret_leases_request_id_unique ON secret_leases (request_id);
CREATE INDEX idx_secret_leases_principal ON secret_leases (requested_by_principal_id);
CREATE INDEX idx_secret_leases_secret_path ON secret_leases (secret_path);
CREATE INDEX idx_secret_leases_status ON secret_leases (status) WHERE status = 'GRANTED';
CREATE INDEX idx_secret_leases_granted_at ON secret_leases (granted_at);

-- ── secret_access_audit_log ──────────────────────────────────────────────────
-- The fourth owned object ("access audit references"). Append-only,
-- mirrors governance_decisions's shape and guarantees exactly: no
-- UPDATE, no DELETE, ever.

CREATE TABLE secret_access_audit_log (
    audit_log_id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- REQUESTED | GRANTED | DENIED | REVOKED | ROTATED — VARCHAR, not enum.
    event_type                 VARCHAR(32) NOT NULL,

    secret_class                VARCHAR(64) NOT NULL,
    secret_path                 TEXT        NOT NULL,

    requested_by_principal_id   TEXT        NOT NULL,
    tenant_id                   UUID,
    legal_entity_id             UUID,

    -- NULL for REQUESTED/DENIED (nothing was granted to reference). Set
    -- for every lease revoked by a ROTATED event too.
    lease_id                    UUID REFERENCES secret_leases(lease_id),

    -- NULL for DENIED when no policy existed at all for that path/scope.
    secret_policy_version_id    UUID REFERENCES secret_policy_versions(secret_policy_version_id),

    -- Only populated (and only deduped) for ROTATED entries — rotation
    -- needs its own idempotency path distinct from secret_leases.request_id
    -- since a rotate call creates no lease row.
    request_id                  TEXT,

    outcome_detail               TEXT,
    correlation_id               TEXT,
    recorded_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_secret_access_audit_log_rotation_request_id
    ON secret_access_audit_log (request_id)
    WHERE event_type = 'ROTATED' AND request_id IS NOT NULL;

CREATE INDEX idx_secret_access_audit_log_principal ON secret_access_audit_log (requested_by_principal_id);
CREATE INDEX idx_secret_access_audit_log_secret_path ON secret_access_audit_log (secret_path);
CREATE INDEX idx_secret_access_audit_log_event_type ON secret_access_audit_log (event_type);
CREATE INDEX idx_secret_access_audit_log_recorded_at ON secret_access_audit_log (recorded_at);
