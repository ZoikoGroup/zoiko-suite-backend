-- 000010_idempotency_keys.up.sql
--
-- Idempotency-Key replay protection (ORG shared contract §3 "Idempotency:
-- replays return the original material result and never duplicate versions";
-- §9.2 DoD gates 2 and 4).
--
-- The envelope has demanded an Idempotency-Key on every material write since
-- the canonical input contract, and nothing ever read one: a retried command
-- ran again (verified live on 28 Sep 2026 — one key, two ChangeDefaultLocale
-- history rows, two version bumps). Same shape as identity-context-svc's
-- 000008, which fixed the same defect there.
--
-- tenant_id has no FK: provisioning is keyed in the platform scope, which is
-- not a row in tenants.

CREATE TABLE idempotency_keys (
    tenant_id           UUID         NOT NULL,
    endpoint            VARCHAR(512) NOT NULL,
    idempotency_key     VARCHAR(255) NOT NULL,
    request_fingerprint VARCHAR(64)  NOT NULL,
    -- 0 while the command is in flight; the HTTP status once it answered.
    response_status     INTEGER      NOT NULL DEFAULT 0,
    response_body       JSONB        NOT NULL DEFAULT 'null'::jsonb,
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, endpoint, idempotency_key),
    CONSTRAINT ik_status_known CHECK (response_status = 0 OR response_status BETWEEN 100 AND 599)
);

CREATE INDEX idx_idempotency_keys_created ON idempotency_keys (created_at);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;

-- Tenant-scoped, plus one named capability for the retention purge, which has
-- no tenant — folded into the one policy the way 000006 folds the outbox
-- relay's, so WITH CHECK still pins every write to the caller's tenant.
-- NULLIF for the reused-connection empty-GUC trap (see 000006).
CREATE POLICY tenant_isolation_policy ON idempotency_keys
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID
                OR current_setting('app.idempotency_purge', true) = 'true')
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
