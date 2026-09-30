-- Down migration for global-search-svc (DATA-06)
-- Migration: 000001_initial_schema.down.sql

DROP TRIGGER IF EXISTS trigger_search_index_transitions ON search_indexes;
DROP FUNCTION IF EXISTS enforce_search_index_transitions();

DROP INDEX IF EXISTS idx_outbox_events_unpublished;
DROP INDEX IF EXISTS idx_index_documents_source_updated;
DROP INDEX IF EXISTS idx_index_documents_scope;
DROP INDEX IF EXISTS idx_index_documents_tsv;

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
DROP POLICY IF EXISTS index_documents_tenant_isolation ON index_documents;
DROP POLICY IF EXISTS search_policies_tenant_isolation ON search_policies;
DROP POLICY IF EXISTS search_indexes_tenant_isolation ON search_indexes;

ALTER TABLE outbox_events DISABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys DISABLE ROW LEVEL SECURITY;
ALTER TABLE index_documents DISABLE ROW LEVEL SECURITY;
ALTER TABLE search_policies DISABLE ROW LEVEL SECURITY;
ALTER TABLE search_indexes DISABLE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS index_documents;
DROP TABLE IF EXISTS search_policies;
DROP TABLE IF EXISTS search_indexes;
