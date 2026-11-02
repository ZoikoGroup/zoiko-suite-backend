-- 000004_outbox_rls.down.sql — restores the 000002/000003 policy shapes.

DROP POLICY IF EXISTS anomaly_outbox_tenant_isolation ON anomaly_outbox;
ALTER TABLE anomaly_outbox NO FORCE ROW LEVEL SECURITY;
ALTER TABLE anomaly_outbox DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS anomaly_models_tenant_isolation ON anomaly_models;
CREATE POLICY anomaly_models_tenant_isolation ON anomaly_models
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS detection_runs_tenant_isolation ON detection_runs;
CREATE POLICY detection_runs_tenant_isolation ON detection_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS anomaly_signals_tenant_isolation ON anomaly_signals;
CREATE POLICY anomaly_signals_tenant_isolation ON anomaly_signals
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS review_dispositions_tenant_isolation ON review_dispositions;
CREATE POLICY review_dispositions_tenant_isolation ON review_dispositions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
