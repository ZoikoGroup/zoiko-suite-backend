DROP POLICY IF EXISTS tenant_isolation ON settlement_applications;
ALTER TABLE settlement_applications NO FORCE ROW LEVEL SECURITY;
ALTER TABLE settlement_applications DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON payable_open_items;
ALTER TABLE payable_open_items NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payable_open_items DISABLE ROW LEVEL SECURITY;
