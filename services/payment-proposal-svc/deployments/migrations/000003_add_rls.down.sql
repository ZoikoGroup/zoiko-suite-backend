DROP POLICY IF EXISTS tenant_isolation ON proposal_events;
ALTER TABLE proposal_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE proposal_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON proposal_items;
ALTER TABLE proposal_items NO FORCE ROW LEVEL SECURITY;
ALTER TABLE proposal_items DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON payment_proposals;
ALTER TABLE payment_proposals NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payment_proposals DISABLE ROW LEVEL SECURITY;
