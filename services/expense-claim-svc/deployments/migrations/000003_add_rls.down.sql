DROP POLICY IF EXISTS tenant_isolation ON expense_claim_events;
ALTER TABLE expense_claim_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE expense_claim_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON expense_lines;
ALTER TABLE expense_lines NO FORCE ROW LEVEL SECURITY;
ALTER TABLE expense_lines DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON expense_claims;
ALTER TABLE expense_claims NO FORCE ROW LEVEL SECURITY;
ALTER TABLE expense_claims DISABLE ROW LEVEL SECURITY;
