DROP POLICY IF EXISTS outbox_tenant_isolation ON delegation_outbox;
DROP INDEX IF EXISTS idx_delegation_outbox_unpublished;
DROP TABLE IF EXISTS delegation_outbox;
