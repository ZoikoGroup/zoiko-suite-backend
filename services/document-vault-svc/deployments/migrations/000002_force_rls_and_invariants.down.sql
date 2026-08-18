DROP INDEX IF EXISTS idx_documents_tenant_created;
DROP INDEX IF EXISTS idx_document_access_log_document_paged;

ALTER TABLE document_versions DROP CONSTRAINT IF EXISTS document_versions_size_nonnegative;
ALTER TABLE document_versions DROP CONSTRAINT IF EXISTS document_versions_checksum_present;
ALTER TABLE document_versions DROP CONSTRAINT IF EXISTS document_versions_version_positive;
ALTER TABLE documents DROP CONSTRAINT IF EXISTS documents_current_version_positive;
ALTER TABLE document_versions DROP CONSTRAINT IF EXISTS document_versions_created_by_identified;
ALTER TABLE documents DROP CONSTRAINT IF EXISTS documents_created_by_identified;
ALTER TABLE document_access_log DROP CONSTRAINT IF EXISTS document_access_log_principal_identified;

DROP POLICY IF EXISTS tenant_isolation_policy ON document_access_log;
ALTER TABLE document_access_log NO FORCE ROW LEVEL SECURITY;
ALTER TABLE document_access_log DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON document_versions;
ALTER TABLE document_versions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE document_versions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON documents;
ALTER TABLE documents NO FORCE ROW LEVEL SECURITY;
ALTER TABLE documents DISABLE ROW LEVEL SECURITY;
