DROP POLICY IF EXISTS tenant_isolation ON run_events;
ALTER TABLE run_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE run_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON instruction_reconciliation_events;
ALTER TABLE instruction_reconciliation_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE instruction_reconciliation_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON run_instructions;
ALTER TABLE run_instructions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE run_instructions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON payment_runs;
ALTER TABLE payment_runs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payment_runs DISABLE ROW LEVEL SECURITY;
