-- Reverse 000002: status columns accept any string again, and the register
-- reads lose their supporting indexes.
DROP INDEX IF EXISTS idx_legal_holds_tenant_status_started;
DROP INDEX IF EXISTS idx_retention_policies_tenant_effective;
ALTER TABLE legal_holds DROP CONSTRAINT IF EXISTS legal_holds_status_check;
ALTER TABLE retention_policies DROP CONSTRAINT IF EXISTS retention_policies_status_check;
