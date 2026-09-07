-- Migration: 000004_add_principal_credentials.down.sql
--
-- Drops credential material entirely. The policy is dropped before the table
-- only for symmetry with the up migration; DROP TABLE would remove it anyway.

DROP POLICY IF EXISTS tenant_isolation_policy ON principal_credentials;
DROP INDEX IF EXISTS idx_principals_tenant_email_human;
DROP TABLE IF EXISTS principal_credentials;
