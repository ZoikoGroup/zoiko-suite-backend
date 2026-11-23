DROP POLICY IF EXISTS eventing_outbox_tenant_isolation ON eventing_outbox;
ALTER TABLE eventing_outbox NO FORCE ROW LEVEL SECURITY;
ALTER TABLE eventing_outbox DISABLE ROW LEVEL SECURITY;

-- Restores the 000001 policies (USING only) and un-forces RLS. RLS itself
-- stays enabled: 000001 enabled it.
DROP POLICY IF EXISTS reconciliation_jobs_tenant_isolation ON reconciliation_jobs;
CREATE POLICY reconciliation_jobs_tenant_isolation ON reconciliation_jobs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS reconciliation_unmatched_items_tenant_isolation ON reconciliation_unmatched_items;
CREATE POLICY reconciliation_unmatched_items_tenant_isolation ON reconciliation_unmatched_items
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- FORCE on reconciliation_jobs / reconciliation_unmatched_items belongs to
-- 000007_force_rls (on main) and is deliberately left in place here.
