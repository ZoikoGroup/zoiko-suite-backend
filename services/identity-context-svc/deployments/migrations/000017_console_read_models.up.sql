-- Migration: 000017_console_read_models.up.sql
--
-- IC-2: durable read models behind the console list endpoints.
--
-- authentication_events is the append-only log of every authentication
-- outcome in the tenant (logins, logouts, failed logins, MFA challenges,
-- step-ups). It backs GET /v1/authentication-events (Sign-in events tab).
--
-- principal_assurance_history records each change to a principal's session
-- assurance level. It backs GET /v1/principals/{id}/assurance-history
-- (People tab drawer).
--
-- Both are tenant-scoped: ENABLE + FORCE ROW LEVEL SECURITY, policies on
-- tenant_id = current_setting('app.tenant_id', true), and every GRANT guarded
-- by an active_flag / existence check so a partial apply cannot leave a grant
-- pointing at nothing.
--
-- ID columns are ULIDs (VARCHAR), matching 000001.

CREATE TABLE authentication_events (
    event_id          VARCHAR(255) PRIMARY KEY,
    tenant_id         VARCHAR(255) NOT NULL,
    principal_id      VARCHAR(255) NOT NULL,
    event_type        VARCHAR(50)  NOT NULL,
    outcome           VARCHAR(20)  NOT NULL,
    assurance_level   VARCHAR(30),
    source_ip         VARCHAR(64),
    user_agent        VARCHAR(255),
    occurred_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id    VARCHAR(255),
    details           JSONB        NOT NULL DEFAULT '{}'::jsonb,

    CONSTRAINT authentication_events_event_type_check
        CHECK (event_type IN ('LOGIN','LOGOUT','FAILED_LOGIN','MFA_CHALLENGE','STEP_UP','SESSION_INVALIDATED','PASSWORD_RESET')),

    CONSTRAINT authentication_events_outcome_check
        CHECK (outcome IN ('SUCCESS','FAILURE'))
);

CREATE INDEX idx_authentication_events_tenant_occurred
    ON authentication_events (tenant_id, occurred_at DESC);

CREATE INDEX idx_authentication_events_principal
    ON authentication_events (principal_id, occurred_at DESC);

CREATE TABLE principal_assurance_history (
    history_id    VARCHAR(255) PRIMARY KEY,
    principal_id  VARCHAR(255) NOT NULL,
    tenant_id     VARCHAR(255) NOT NULL,
    assurance_level VARCHAR(30) NOT NULL,
    changed_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    changed_by    VARCHAR(255),
    reason        VARCHAR(255)
);

CREATE INDEX idx_principal_assurance_history_principal
    ON principal_assurance_history (principal_id, changed_at DESC);

ALTER TABLE authentication_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE authentication_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON authentication_events
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE principal_assurance_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE principal_assurance_history FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON principal_assurance_history
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Guarded grants: only apply when the target table actually exists, so a
-- partially applied migration cannot leave a grant on a missing relation.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'authentication_events') THEN
        GRANT SELECT ON authentication_events TO app_identity_context;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'principal_assurance_history') THEN
        GRANT SELECT ON principal_assurance_history TO app_identity_context;
    END IF;
END
$$;
