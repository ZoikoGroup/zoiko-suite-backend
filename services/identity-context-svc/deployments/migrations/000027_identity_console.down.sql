-- Migration: 000019_identity_console.down.sql
--
-- Reverts 000019. identity_links comes back empty, exactly as 000018 left it
-- (nothing ever wrote it). Refused while a merge carries a reason, which only
-- 000019 can record.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM identity_merges WHERE reason <> '') THEN
        RAISE EXCEPTION '000019 down refused: identity merges with a recorded reason exist';
    END IF;
END
$$;

DROP INDEX IF EXISTS idx_principals_tenant_created;
DROP INDEX IF EXISTS idx_session_contexts_tenant_issued;

CREATE TABLE identity_links (
    link_id            VARCHAR(255) PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    principal_id       VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    identity_provider  VARCHAR(100) NOT NULL,
    provider_subject   VARCHAR(255) NOT NULL,
    provider_type      VARCHAR(20)  NOT NULL DEFAULT 'OIDC',
    created_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT identity_links_provider_type_check CHECK (provider_type IN ('OIDC','SAML')),
    CONSTRAINT identity_links_one_link_per_provider_subject UNIQUE (tenant_id, identity_provider, provider_subject)
);
CREATE INDEX idx_identity_links_principal ON identity_links (principal_id);
CREATE INDEX idx_identity_links_tenant_provider_subject ON identity_links (tenant_id, identity_provider, provider_subject);
ALTER TABLE identity_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON identity_links
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP INDEX IF EXISTS idx_identity_merges_survivor;
DROP INDEX IF EXISTS uq_identity_merges_retired;
ALTER TABLE identity_merges DROP COLUMN IF EXISTS reason;

DROP TRIGGER IF EXISTS identity_merges_append_only ON identity_merges;
DROP TRIGGER IF EXISTS principal_assurance_history_append_only ON principal_assurance_history;
DROP TRIGGER IF EXISTS authentication_events_append_only ON authentication_events;
DROP FUNCTION IF EXISTS identity_append_only();

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_identity_context') THEN
        REVOKE INSERT ON authentication_events, principal_assurance_history FROM app_identity_context;
        GRANT SELECT, INSERT, UPDATE, DELETE ON identity_links TO app_identity_context;
    END IF;
END
$$;
