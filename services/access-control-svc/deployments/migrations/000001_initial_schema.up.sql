-- 000001_initial_schema.up.sql
-- Access Control Service — initial schema
--
-- Owns: roles, permission_bundles, role_permission_bundle_links.
--
-- Note on PrincipalRoleAssignment ownership:
--   This table is CREATED here (access-control-svc owns its schema) but
--   its WRITE PATH belongs to authorization-svc, which publishes
--   role.assigned events consumed by identity-context-svc's event consumer.
--   Access Control Service reads it read-only for bundle-expansion queries.
--
-- Design decisions aligned with ZoikoSuite doctrine:
--   - All status/type columns are VARCHAR — no pg enums. New values are
--     data-only changes, zero code/redeploy required per doctrine §3.9.
--   - No hard-delete. Roles and bundles deactivate via active_flag.
--     active_flag is set FALSE instead of DELETE — keeps audit trail.
--   - Idempotency: CREATE ROLE uses ON CONFLICT DO NOTHING on
--     (tenant_id, role_code); CREATE BUNDLE uses ON CONFLICT UPDATE on
--     (tenant_id, bundle_code) to allow permitted_actions to be updated
--     in a safe, deterministic manner.
--   - Every table carries tenant_id. Row-Level Security is enabled and
--     enforced via a RLS policy (same pattern as tenant-entity-registry-svc).

-- ── roles ─────────────────────────────────────────────────────────────────────

CREATE TABLE roles (
    role_id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID         NOT NULL,

    -- Idempotent creation dedup key; unique within a tenant.
    role_code                VARCHAR(128) NOT NULL,
    role_name                TEXT         NOT NULL,

    -- Data only (e.g. "TENANT", "LEGAL_ENTITY", "GLOBAL").
    role_scope_type          VARCHAR(32)  NOT NULL,

    -- Deactivation instead of DELETE per no-soft-delete doctrine.
    active_flag              BOOLEAN      NOT NULL DEFAULT TRUE,

    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT         NOT NULL
);

CREATE UNIQUE INDEX idx_roles_tenant_code ON roles (tenant_id, role_code);
CREATE INDEX idx_roles_tenant ON roles (tenant_id);

-- RLS: each tenant sees only its own roles.
ALTER TABLE roles ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON roles
    USING (tenant_id = current_setting('app.current_tenant_id', TRUE)::UUID);

-- ── permission_bundles ────────────────────────────────────────────────────────

CREATE TABLE permission_bundles (
    permission_bundle_id  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID         NOT NULL,

    -- Idempotent creation dedup key; unique within a tenant.
    bundle_code           VARCHAR(128) NOT NULL,
    bundle_name           TEXT         NOT NULL,

    -- JSON array of action-type strings this bundle grants, e.g.
    -- ["PAYMENT_APPROVE", "PAYMENT_VIEW", "JOURNAL_POST"].
    -- Updated atomically via ON CONFLICT UPDATE — no partial writes.
    permitted_actions     JSONB        NOT NULL DEFAULT '[]',

    active_flag           BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_bundles_tenant_code ON permission_bundles (tenant_id, bundle_code);
CREATE INDEX idx_bundles_tenant ON permission_bundles (tenant_id);
CREATE INDEX idx_bundles_actions ON permission_bundles USING GIN (permitted_actions);

ALTER TABLE permission_bundles ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON permission_bundles
    USING (tenant_id = current_setting('app.current_tenant_id', TRUE)::UUID);

-- ── role_permission_bundle_links ──────────────────────────────────────────────
-- Many-to-many: which bundles a role carries.
-- A role can have multiple bundles; a bundle can be reused across roles.
-- The union of all active bundles' permitted_actions is the effective
-- permission set for a role (resolved by authorization-svc at evaluation time).

CREATE TABLE role_permission_bundle_links (
    link_id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    role_id               UUID         NOT NULL REFERENCES roles(role_id),
    permission_bundle_id  UUID         NOT NULL REFERENCES permission_bundles(permission_bundle_id),

    -- Deactivation: set active_flag FALSE instead of DELETE.
    active_flag           BOOLEAN      NOT NULL DEFAULT TRUE,

    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by            TEXT         NOT NULL,

    UNIQUE (role_id, permission_bundle_id)
);

CREATE INDEX idx_links_role ON role_permission_bundle_links (role_id) WHERE active_flag;
CREATE INDEX idx_links_bundle ON role_permission_bundle_links (permission_bundle_id) WHERE active_flag;

-- ── event_outbox ──────────────────────────────────────────────────────────────
-- Transactional outbox for reliable event publishing.
-- Events are written in the same transaction as the domain change; a
-- background publisher picks them up and marks them delivered.
-- This ensures the ZoikoSuite guarantee: events are facts, never lost.

CREATE TABLE event_outbox (
    outbox_id        UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type       VARCHAR(128) NOT NULL,
    topic            VARCHAR(255) NOT NULL,
    payload          JSONB        NOT NULL,
    correlation_id   TEXT,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    delivered_at     TIMESTAMPTZ,
    delivery_attempts INT         NOT NULL DEFAULT 0
);

CREATE INDEX idx_outbox_undelivered ON event_outbox (created_at) WHERE delivered_at IS NULL;
