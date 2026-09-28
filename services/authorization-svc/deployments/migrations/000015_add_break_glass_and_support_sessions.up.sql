-- Migration: 000015_add_break_glass_and_support_sessions.up.sql
--
-- Adds Break-Glass Emergency Sessions (ZS-IAM-001 §14 & §21) and
-- Tenant Support / Impersonation Sessions (ZS-IAM-001 §15 & §21).

BEGIN;

CREATE TABLE break_glass_sessions (
    session_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    principal_id       TEXT NOT NULL,
    incident_id        TEXT NOT NULL,
    reason             TEXT NOT NULL,
    requested_actions  TEXT[] NOT NULL DEFAULT '{}',
    status             VARCHAR(32) NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'REVOKED', 'EXPIRED')),
    duration_seconds   INT NOT NULL DEFAULT 1800,
    expires_at         TIMESTAMPTZ NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at         TIMESTAMPTZ,
    revoked_by         TEXT
);

COMMENT ON TABLE break_glass_sessions IS
    'Emergency break-glass access sessions (ZS-IAM-001 §14 & §21). High-assurance, time-bound emergency elevation triggered only by a declared incident.';

COMMENT ON COLUMN break_glass_sessions.incident_id IS
    'Mandatory declared incident identifier. Scenario A16: break-glass without declared incident is strictly prohibited.';

CREATE INDEX idx_break_glass_sessions_lookup
    ON break_glass_sessions (tenant_id, principal_id, status, expires_at);

CREATE INDEX idx_break_glass_sessions_active
    ON break_glass_sessions (session_id)
    WHERE status = 'ACTIVE';

ALTER TABLE break_glass_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE break_glass_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON break_glass_sessions
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);


CREATE TABLE support_sessions (
    session_id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    support_operator_id      TEXT NOT NULL,
    ticket_ref               TEXT NOT NULL,
    purpose                  TEXT NOT NULL,
    read_only                BOOLEAN NOT NULL DEFAULT TRUE,
    allow_bulk_export        BOOLEAN NOT NULL DEFAULT FALSE,
    allowed_actions          TEXT[] NOT NULL DEFAULT '{}',
    status                   VARCHAR(32) NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'REVOKED', 'EXPIRED')),
    duration_seconds         INT NOT NULL DEFAULT 3600,
    expires_at               TIMESTAMPTZ NOT NULL,
    tenant_consent_obtained  BOOLEAN NOT NULL DEFAULT TRUE,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at               TIMESTAMPTZ,
    revoked_by               TEXT
);

COMMENT ON TABLE support_sessions IS
    'Controlled tenant support sessions (ZS-IAM-001 §15 & §21). Purpose-bound, auditable support access with explicit operator attribution.';

COMMENT ON COLUMN support_sessions.allow_bulk_export IS
    'Whether bulk export is permitted. Scenario A14: Support user bulk export is denied by default unless explicitly granted.';

CREATE INDEX idx_support_sessions_lookup
    ON support_sessions (tenant_id, support_operator_id, status, expires_at);

CREATE INDEX idx_support_sessions_active
    ON support_sessions (session_id)
    WHERE status = 'ACTIVE';

ALTER TABLE support_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE support_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON support_sessions
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
