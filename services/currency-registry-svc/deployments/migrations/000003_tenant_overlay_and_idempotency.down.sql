DROP POLICY IF EXISTS tenant_isolation_policy ON currency_idempotency;
DROP TABLE IF EXISTS currency_idempotency;
DROP TRIGGER IF EXISTS trg_tenant_currency_support_no_delete ON tenant_currency_support;
DROP POLICY IF EXISTS tenant_isolation_policy ON tenant_currency_support;
DROP TABLE IF EXISTS tenant_currency_support;
