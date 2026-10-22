-- Idempotency for POST /v1/archives.
--
-- The canonical service input contract (ZS-ARCH-SVC-001 §4, INV-08) requires
-- Idempotency-Key on material writes. This service required the header but
-- nothing read it, so a retried POST /v1/archives wrote a second, identical
-- archive record. Archive records are permanent (audit_archives is
-- append-only), so a duplicate can never be removed afterwards.
--
-- Same shape as the pattern this repository already uses for this exact
-- problem (identity-context-svc migration 000008, ported into
-- search-indexer-svc migration 000003): a (tenant, endpoint, key) primary key,
-- a request fingerprint that is bound to the acting principal, and the first
-- response kept so a retry is answered with it.
--
-- One deliberate difference from that pattern: the claim and the recorded
-- response are written in the SAME transaction as the archive row (see
-- PgStore.CreateArchive). The row therefore never exists in an "in flight"
-- state. A failed archive (a broken chain, an empty range, any error) rolls
-- the claim back with it, so a failure can never leave a false replay record;
-- and a concurrent duplicate blocks on the unique key until the first request
-- commits, then replays its result instead of inserting a second archive.
--
-- Tenant-scoped absolutely, like audit_archives itself (migration 000005):
-- a stored response is a response that tenant received. There is deliberately
-- no platform-scope branch.
--
-- Rows written by a request that carried no Idempotency-Key do not exist; the
-- key is optional at the service boundary only when the envelope middleware is
-- not in its write-strict mode.

CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id            TEXT         NOT NULL,
    endpoint             TEXT         NOT NULL,
    idempotency_key      VARCHAR(255) NOT NULL,
    request_fingerprint  TEXT         NOT NULL,
    response_status      INTEGER      NOT NULL DEFAULT 0,
    response_body        JSONB        NOT NULL DEFAULT 'null'::jsonb,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT idempotency_keys_pk PRIMARY KEY (tenant_id, endpoint, idempotency_key),
    CONSTRAINT idempotency_keys_status_sane CHECK (response_status = 0 OR response_status BETWEEN 100 AND 599)
);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS idempotency_tenant_isolation_policy ON idempotency_keys;
CREATE POLICY idempotency_tenant_isolation_policy ON idempotency_keys
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
