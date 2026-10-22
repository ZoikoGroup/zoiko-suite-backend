-- Migration: 000005_add_archive_tenant_isolation.down.sql

DROP POLICY IF EXISTS archive_verification_tenant_isolation_policy ON audit_archive_verifications;
ALTER TABLE audit_archive_verifications NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_archive_verifications DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS archive_tenant_isolation_policy ON audit_archives;
ALTER TABLE audit_archives NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_archives DISABLE ROW LEVEL SECURITY;

DROP INDEX IF EXISTS audit_archive_verifications_tenant_idx;
DROP INDEX IF EXISTS audit_archives_tenant_idx;

ALTER TABLE audit_archive_verifications DROP COLUMN IF EXISTS tenant_id;
ALTER TABLE audit_archives DROP COLUMN IF EXISTS tenant_id;
