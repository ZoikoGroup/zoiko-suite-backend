DROP POLICY IF EXISTS tenant_isolation_policy ON transfer_decisions;
ALTER TABLE transfer_decisions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON transfer_assessments;
ALTER TABLE transfer_assessments DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON transfer_mechanisms;
ALTER TABLE transfer_mechanisms DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON subprocessors;
ALTER TABLE subprocessors DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON processor_relationships;
ALTER TABLE processor_relationships DISABLE ROW LEVEL SECURITY;
