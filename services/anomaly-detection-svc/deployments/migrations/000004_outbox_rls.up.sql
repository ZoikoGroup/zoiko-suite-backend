-- 000004_outbox_rls.up.sql
--
-- 1. anomaly_outbox joins every other table of this service under ENABLE +
--    FORCE ROW LEVEL SECURITY. Request-path writes (store.enqueueEvent) run
--    inside the command's tenant transaction and are confined to that tenant.
--    The relay (internal/outbox) is not a request: it drains every tenant's
--    backlog, so it installs app.outbox_relay = 'true' and is admitted by an
--    explicit, auditable disjunct instead of running unscoped (which under
--    FORCE RLS would see nothing). Same pattern as accounting-period-svc and
--    access-control-svc.
--
--    NULLIF: a pooled connection keeps a custom GUC as '' after a
--    transaction-local set_config resets, so '' must mean "not set".
--
-- 2. The AI-04 tables created in 000002 get an explicit WITH CHECK (USING
--    alone already implies it for FOR ALL; the platform convention spells it
--    out, as 000003 did for the rule-engine tables).

ALTER TABLE anomaly_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE anomaly_outbox FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS anomaly_outbox_tenant_isolation ON anomaly_outbox;
CREATE POLICY anomaly_outbox_tenant_isolation ON anomaly_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );

DROP POLICY IF EXISTS anomaly_models_tenant_isolation ON anomaly_models;
CREATE POLICY anomaly_models_tenant_isolation ON anomaly_models FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS detection_runs_tenant_isolation ON detection_runs;
CREATE POLICY detection_runs_tenant_isolation ON detection_runs FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS anomaly_signals_tenant_isolation ON anomaly_signals;
CREATE POLICY anomaly_signals_tenant_isolation ON anomaly_signals FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS review_dispositions_tenant_isolation ON review_dispositions;
CREATE POLICY review_dispositions_tenant_isolation ON review_dispositions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
