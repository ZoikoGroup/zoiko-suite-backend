-- Migration: 000006_add_shared_secret_exceptions.up.sql
--
-- The shared-secret exception register (compliance audit §13 "no
-- shared-secret exception register"). A deliberate, evidence-backed,
-- time-boxed override: registered by an operator with a reason and an
-- evidence reference, auto-expiring, and revocable. Append-mostly —
-- status moves ACTIVE -> EXPIRED (auto, computed read) or ACTIVE ->
-- REVOKED (explicit), never a hard delete.
--
-- tenant_id NULL means a global exception (visible to all tenants),
-- non-NULL means tenant-scoped. RLS mirrors 000003 so tenants only see
-- global rows plus their own; the platform-scope / secret_vault_platform_scope()
-- escape hatch lets the registering operator (platform-scoped by
-- authorization) write/read across scopes.

CREATE TABLE shared_secret_exceptions (
    exception_id            UUID PRIMARY KEY,
    secret_path             TEXT NOT NULL,
    reason                  TEXT NOT NULL,
    evidence_reference      TEXT NOT NULL,
    approved_by_principal_id TEXT NOT NULL,
    tenant_id               UUID,
    expires_at              TIMESTAMPTZ NOT NULL,
    status                  TEXT NOT NULL DEFAULT 'ACTIVE'
                            CHECK (status IN ('ACTIVE', 'REVOKED', 'EXPIRED')),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at              TIMESTAMPTZ,
    revoked_by_principal_id TEXT
);

-- At most one ACTIVE exception per secret path (a second override of the
-- same material must supersede, not stack).
CREATE UNIQUE INDEX idx_shared_secret_exceptions_one_active_per_path
    ON shared_secret_exceptions (secret_path, tenant_id)
    WHERE status = 'ACTIVE';

CREATE INDEX idx_shared_secret_exceptions_status
    ON shared_secret_exceptions (status);

ALTER TABLE shared_secret_exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE shared_secret_exceptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON shared_secret_exceptions
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR secret_vault_platform_scope()
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR secret_vault_platform_scope()
    );