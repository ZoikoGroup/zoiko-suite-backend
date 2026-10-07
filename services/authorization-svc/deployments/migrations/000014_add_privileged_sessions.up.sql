-- Migration: 000014_add_privileged_sessions.up.sql
--
-- Adds Just-in-Time (JIT) privileged access management sessions (ZS-IAM-001 §13 & §21).
-- Allows time-bound, ticket-referenced elevation of capabilities with audit preservation.

BEGIN;

CREATE TABLE privileged_sessions (
    session_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    principal_id       TEXT NOT NULL,
    requested_actions  TEXT[] NOT NULL DEFAULT '{}',
    ticket_ref         TEXT NOT NULL,
    reason             TEXT NOT NULL,
    status             VARCHAR(32) NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'REVOKED', 'EXPIRED')),
    duration_seconds   INT NOT NULL DEFAULT 3600,
    expires_at         TIMESTAMPTZ NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at         TIMESTAMPTZ,
    revoked_by         TEXT
);

COMMENT ON TABLE privileged_sessions IS
    'Just-in-Time (JIT) privileged access sessions (ZS-IAM-001 §13 & §21). Time-bound elevation granted with explicit ticket reference and purpose.';

COMMENT ON COLUMN privileged_sessions.status IS
    'Privileged session status: ACTIVE | REVOKED | EXPIRED.';

CREATE INDEX idx_privileged_sessions_lookup
    ON privileged_sessions (tenant_id, principal_id, status, expires_at);

CREATE INDEX idx_privileged_sessions_active
    ON privileged_sessions (session_id)
    WHERE status = 'ACTIVE';

ALTER TABLE privileged_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE privileged_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON privileged_sessions
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
