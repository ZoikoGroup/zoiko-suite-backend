DROP POLICY IF EXISTS tenant_isolation ON recovery_commitments;
ALTER TABLE recovery_commitments NO FORCE ROW LEVEL SECURITY;
ALTER TABLE recovery_commitments DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON recovery_applications;
ALTER TABLE recovery_applications NO FORCE ROW LEVEL SECURITY;
ALTER TABLE recovery_applications DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON supplier_recovery_cases;
ALTER TABLE supplier_recovery_cases NO FORCE ROW LEVEL SECURITY;
ALTER TABLE supplier_recovery_cases DISABLE ROW LEVEL SECURITY;
