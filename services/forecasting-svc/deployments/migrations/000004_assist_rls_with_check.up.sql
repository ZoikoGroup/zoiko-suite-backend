-- 000004_assist_rls_with_check.up.sql
--
-- The AI-05 tables' RLS policies from 000002 had USING only. With FOR ALL and
-- no WITH CHECK, Postgres applies USING to new rows too, but the platform
-- standard (and 000003 for forecast_models / forecast_projections /
-- idempotency_keys) states the write check explicitly, so a row can never be
-- written into a tenant other than the one declared for the transaction.
-- Idempotent: each policy is dropped and recreated.

DROP POLICY IF EXISTS forecast_model_releases_tenant_isolation ON forecast_model_releases;
CREATE POLICY forecast_model_releases_tenant_isolation ON forecast_model_releases
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS forecast_assist_jobs_tenant_isolation ON forecast_assist_jobs;
CREATE POLICY forecast_assist_jobs_tenant_isolation ON forecast_assist_jobs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS suggested_drivers_tenant_isolation ON suggested_drivers;
CREATE POLICY suggested_drivers_tenant_isolation ON suggested_drivers
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS suggested_ranges_tenant_isolation ON suggested_ranges;
CREATE POLICY suggested_ranges_tenant_isolation ON suggested_ranges
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS evidence_references_tenant_isolation ON evidence_references;
CREATE POLICY evidence_references_tenant_isolation ON evidence_references
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS planner_decisions_tenant_isolation ON planner_decisions;
CREATE POLICY planner_decisions_tenant_isolation ON planner_decisions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
