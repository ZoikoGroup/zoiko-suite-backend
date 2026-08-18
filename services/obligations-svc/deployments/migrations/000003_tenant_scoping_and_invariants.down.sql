BEGIN;

ALTER TABLE filing_requirements DROP CONSTRAINT IF EXISTS filing_requirements_status_known;
ALTER TABLE obligations DROP CONSTRAINT IF EXISTS obligations_closed_has_timestamp;
ALTER TABLE obligations DROP CONSTRAINT IF EXISTS obligations_status_known;

DROP INDEX IF EXISTS idx_filing_requirements_tenant;
DROP INDEX IF EXISTS idx_obligations_tenant_due_id;
DROP INDEX IF EXISTS idx_obligations_tenant_due;
DROP INDEX IF EXISTS idx_obligations_tenant_status;
DROP INDEX IF EXISTS idx_obligations_tenant_entity;

DROP POLICY IF EXISTS tenant_isolation_policy ON filing_requirements;
DROP POLICY IF EXISTS tenant_isolation_policy ON obligations;
ALTER TABLE filing_requirements NO FORCE ROW LEVEL SECURITY;
ALTER TABLE filing_requirements DISABLE ROW LEVEL SECURITY;
ALTER TABLE obligations NO FORCE ROW LEVEL SECURITY;
ALTER TABLE obligations DISABLE ROW LEVEL SECURITY;

-- Restoring the GLOBAL unique index is what makes this a real rollback, and it
-- reinstates the cross-tenant replay 000002 fixed. It can also fail outright:
-- once two tenants each hold the same obligation_code, no global unique index
-- can be built. That is not a flaw in the down migration — it is the schema
-- telling you the data has moved past the point where the old shape was valid.
DROP INDEX IF EXISTS idx_obligations_tenant_code_unique;
CREATE UNIQUE INDEX idx_obligations_code_unique ON obligations (obligation_code);

ALTER TABLE filing_requirements DROP COLUMN IF EXISTS tenant_id;
ALTER TABLE obligations DROP COLUMN IF EXISTS tenant_id;

COMMIT;
