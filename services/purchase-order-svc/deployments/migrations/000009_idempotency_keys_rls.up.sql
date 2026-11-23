-- idempotency_keys joins the service's other tables under ENABLE + FORCE ROW
-- LEVEL SECURITY.
--
-- 000007 created it with a tenant column (tenant_key, the verified X-Tenant-Id
-- the handler scoped the key to) but no row-level security, and the store
-- reached it through bare pool statements with no app.tenant_id set. Every
-- other tenant-scoped table here is FORCE'd; this one held stored response
-- bodies of every tenant's commands with nothing but the store's own WHERE
-- clause between them.
--
-- The store now runs each statement inside a transaction that sets
-- app.tenant_id (withRLS), so the request path is confined to its tenant.
--
-- The retention purge has no tenant: it deletes expired keys of every tenant.
-- It declares app.idempotency_purge = 'true' in its own transaction, and that
-- capability is admitted on USING only (read/delete), never on WITH CHECK, so
-- no insert or update can be written outside the caller's tenant. Same shape
-- as tenant-entity-registry-svc's 000010 and this service's outbox relay
-- disjunct (000008).
--
-- tenant_key is TEXT (it holds the header value verbatim), so the comparison
-- is text = text. NULLIF: a pooled connection keeps a custom GUC as '' after a
-- transaction-local set_config resets, so '' must mean "not set".
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys FOR ALL
    USING (
        tenant_key = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.idempotency_purge', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_key = NULLIF(current_setting('app.tenant_id', true), '')
    );
