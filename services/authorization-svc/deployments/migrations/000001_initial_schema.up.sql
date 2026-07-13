-- 000001_initial_schema.up.sql
-- Authorization Service — initial schema
--
-- Owns: roles, permission_bundles, principal_role_assignments,
-- delegated_authorities, sod_rules, access_decision_log.
--
-- Design decisions:
--   - role_scope_type, scope_type, authority_limit_type, conflict_type are
--     all VARCHAR — no enums. New values are added via data only.
--   - revocation_status IS a real (tiny) state machine: ACTIVE -> REVOKED,
--     enforced in application code, one-way, never re-activated.
--   - No hard-delete anywhere. Roles/bundles deactivate via active_flag;
--     role assignments end via effective_to; delegated authorities end via
--     revocation_status. access_decision_log is pure append-only — no
--     UPDATE or DELETE statement should ever target it.
--   - Critical constraint: "no material action executes without an
--     authorization decision artifact" — access_decision_log has no
--     nullable decision_outcome/decision_basis; every evaluation, granted
--     or denied, is written here before the caller gets a response.

-- ── roles ────────────────────────────────────────────────────────────────────

CREATE TABLE roles (
    role_id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID        NOT NULL,

    -- Idempotent creation dedup key, unique within a tenant.
    role_code                VARCHAR(128) NOT NULL,
    role_name                TEXT        NOT NULL,

    -- Data only (e.g. "TENANT", "LEGAL_ENTITY").
    role_scope_type          VARCHAR(32) NOT NULL,

    active_flag              BOOLEAN     NOT NULL DEFAULT TRUE,

    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT        NOT NULL
);

CREATE UNIQUE INDEX idx_roles_tenant_code_unique ON roles (tenant_id, role_code);
CREATE INDEX idx_roles_tenant ON roles (tenant_id);

-- ── permission_bundles ───────────────────────────────────────────────────────

CREATE TABLE permission_bundles (
    permission_bundle_id     UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    role_id                  UUID        NOT NULL REFERENCES roles(role_id),

    bundle_code              VARCHAR(128) NOT NULL,

    -- JSON array of action-type strings this bundle grants, e.g.
    -- ["PAYMENT_APPROVE", "PAYMENT_VIEW"].
    permitted_actions        JSONB       NOT NULL DEFAULT '[]',

    active_flag              BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_permission_bundles_role_code_unique ON permission_bundles (role_id, bundle_code);
CREATE INDEX idx_permission_bundles_role ON permission_bundles (role_id);

-- ── principal_role_assignments ───────────────────────────────────────────────

CREATE TABLE principal_role_assignments (
    principal_role_assignment_id UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    principal_id                 TEXT        NOT NULL,
    role_id                      UUID        NOT NULL REFERENCES roles(role_id),
    legal_entity_id               UUID        NOT NULL,

    effective_from                TIMESTAMPTZ NOT NULL,
    effective_to                  TIMESTAMPTZ,

    assigned_by                   TEXT        NOT NULL,
    created_at                    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Primary evaluation lookup: what roles does this principal hold in this entity, right now?
CREATE INDEX idx_assignments_lookup
    ON principal_role_assignments (principal_id, legal_entity_id, effective_from, effective_to);

-- ── delegated_authorities ────────────────────────────────────────────────────

CREATE TABLE delegated_authorities (
    delegated_authority_id   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    delegator_principal_id   TEXT        NOT NULL,
    delegate_principal_id    TEXT        NOT NULL,

    -- Data only (e.g. "FULL", "ACTION_SUBSET").
    scope_type               VARCHAR(64) NOT NULL,
    legal_entity_id          UUID        NOT NULL,

    authority_limit_type     VARCHAR(64),
    authority_limit_value    TEXT,

    effective_from           TIMESTAMPTZ NOT NULL,
    effective_to             TIMESTAMPTZ,

    -- ACTIVE | REVOKED. One-way transition, enforced in application code.
    revocation_status        VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',

    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Primary evaluation lookup: who has delegated to this principal, in this entity, right now?
CREATE INDEX idx_delegations_lookup
    ON delegated_authorities (delegate_principal_id, legal_entity_id, revocation_status);

-- ── sod_rules ─────────────────────────────────────────────────────────────────

CREATE TABLE sod_rules (
    sod_rule_id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    domain_code              VARCHAR(64)  NOT NULL,
    action_a                 VARCHAR(128) NOT NULL,
    action_b                 VARCHAR(128) NOT NULL,

    -- Data only (e.g. "MUTUALLY_EXCLUSIVE").
    conflict_type            VARCHAR(64) NOT NULL,

    -- NULL = globally-applicable rule, not jurisdiction-specific.
    jurisdiction_id          UUID,

    active_flag              BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Evaluation lookup: any rule where the candidate action appears on either side.
CREATE INDEX idx_sod_rules_action_a ON sod_rules (action_a) WHERE active_flag;
CREATE INDEX idx_sod_rules_action_b ON sod_rules (action_b) WHERE active_flag;

-- ── access_decision_log ──────────────────────────────────────────────────────
-- Append-only evidence. No UPDATE/DELETE statement should ever target this table.

CREATE TABLE access_decision_log (
    access_decision_id       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    principal_id             TEXT        NOT NULL,
    legal_entity_id          UUID        NOT NULL,
    action_type              VARCHAR(128) NOT NULL,

    -- GRANTED | DENIED.
    decision_outcome         VARCHAR(16) NOT NULL,

    -- Human-readable explanation of which layer produced the outcome —
    -- never just "denied" with no reason.
    decision_basis           TEXT        NOT NULL,

    correlation_id           TEXT,
    decided_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Rationale retrieval and audit queries.
CREATE INDEX idx_access_decision_log_principal ON access_decision_log (principal_id, decided_at DESC);
CREATE INDEX idx_access_decision_log_entity ON access_decision_log (legal_entity_id, decided_at DESC);
