-- 000036_async_operations.up.sql
--
-- API-5 (tracker EST-5: "jurisdiction-rules compile and test runs"): a pack
-- compile or test run asked for with `Prefer: respond-async` answers 202 with
-- Location: /v1/operations/{id} and runs in the background; the caller polls the
-- operation and may cancel it before it starts. The table is the shared contract
-- of identity-envelope/apicontract/asyncpgx (same columns, same semantics), with
-- tenant row-level security as every tenant table here (R4): an operation is
-- readable only by the tenant that started it.

CREATE TABLE IF NOT EXISTS async_operations (
    id                  text PRIMARY KEY,
    tenant_id           text NOT NULL,
    principal_id        text NOT NULL DEFAULT '',
    route               text NOT NULL DEFAULT '',
    type                text NOT NULL,
    state               text NOT NULL CHECK (state IN
        ('queued','running','waiting_external','succeeded','failed','cancel_requested','cancelled')),
    progress_completed  bigint NOT NULL DEFAULT 0 CHECK (progress_completed >= 0),
    progress_total      bigint NOT NULL DEFAULT 0 CHECK (progress_total >= 0),
    resource_ref        text NOT NULL DEFAULT '',
    problem             jsonb,
    cancelable          boolean NOT NULL DEFAULT true,
    idempotency_key     text NOT NULL DEFAULT '',
    request_hash        text NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    started_at          timestamptz,
    finished_at         timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS async_operations_idempotency_idx
    ON async_operations (tenant_id, principal_id, route, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX IF NOT EXISTS async_operations_tenant_created_idx
    ON async_operations (tenant_id, created_at DESC);

ALTER TABLE async_operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE async_operations FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS async_operations_tenant_isolation ON async_operations;
CREATE POLICY async_operations_tenant_isolation ON async_operations
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT, UPDATE ON async_operations TO app_jurisdiction_rules;
    END IF;
END $$;
