DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
ALTER TABLE idempotency_keys NO FORCE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys DISABLE ROW LEVEL SECURITY;
