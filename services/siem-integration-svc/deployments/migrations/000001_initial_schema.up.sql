-- siem-integration-svc: exporters and the security event stream (tracker rows
-- 8p / 89 — both previously lived in an in-memory map, so a restart erased the
-- tenant's SIEM destinations and every security event recorded so far).
--
-- Authoritative here:
--   * siem_exporters — a tenant's SIEM destinations. The platform credential
--     (auth_token) is NEVER stored in plaintext: auth_token_ciphertext holds an
--     AES-256-GCM seal (internal/credential, key outside the database), bound to
--     (tenant, exporter) by AAD; auth_token_key_id names the sealing key. Both
--     NULL when no token was supplied. The token is never returned on any read.
--   * siem_events — security events streamed by platform services. Doc 05 §13.4
--     requires security logs to be append-oriented: rows may be inserted, never
--     updated or deleted (trigger below), whoever connects.

CREATE TABLE IF NOT EXISTS siem_exporters (
    id                    UUID         PRIMARY KEY,
    tenant_id             VARCHAR(255) NOT NULL,
    legal_entity_id       VARCHAR(255) NOT NULL,
    name                  VARCHAR(255) NOT NULL,
    platform              VARCHAR(16)  NOT NULL CHECK (platform IN ('SPLUNK', 'DATADOG', 'ELASTIC', 'SENTINEL', 'SYSLOG')),
    endpoint_url          TEXT         NOT NULL,
    auth_token_ciphertext TEXT,
    auth_token_key_id     VARCHAR(64),
    status                VARCHAR(16)  NOT NULL CHECK (status IN ('ACTIVE', 'PAUSED', 'DISABLED')),
    events_sent           BIGINT       NOT NULL DEFAULT 0 CHECK (events_sent >= 0),
    last_streamed         TIMESTAMPTZ,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CHECK ((auth_token_ciphertext IS NULL) = (auth_token_key_id IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_siem_exporters_scope ON siem_exporters (tenant_id, legal_entity_id, status);

CREATE TABLE IF NOT EXISTS siem_events (
    id             UUID         PRIMARY KEY,
    tenant_id      VARCHAR(255) NOT NULL,
    exporter_id    UUID         NOT NULL REFERENCES siem_exporters (id),
    source_service VARCHAR(255) NOT NULL,
    event_type     VARCHAR(255) NOT NULL,
    severity       VARCHAR(16)  NOT NULL CHECK (severity IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL')),
    message        TEXT         NOT NULL,
    payload        TEXT         NOT NULL DEFAULT '',
    status         VARCHAR(16)  NOT NULL CHECK (status IN ('DELIVERED', 'FAILED')),
    occurred_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_siem_events_exporter ON siem_events (tenant_id, exporter_id, occurred_at);

CREATE OR REPLACE FUNCTION siem_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'siem_events is append-only (% refused)', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_siem_events_append_only ON siem_events;
CREATE TRIGGER trg_siem_events_append_only
    BEFORE UPDATE OR DELETE ON siem_events
    FOR EACH ROW EXECUTE FUNCTION siem_events_append_only();

ALTER TABLE siem_exporters ENABLE ROW LEVEL SECURITY;
ALTER TABLE siem_exporters FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS siem_exporters_tenant_isolation ON siem_exporters;
CREATE POLICY siem_exporters_tenant_isolation ON siem_exporters FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE siem_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE siem_events FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS siem_events_tenant_isolation ON siem_events;
CREATE POLICY siem_events_tenant_isolation ON siem_events FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
