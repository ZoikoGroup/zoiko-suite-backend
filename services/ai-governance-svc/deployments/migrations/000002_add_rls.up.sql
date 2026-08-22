-- 000002_add_rls.up.sql
--
-- Row-level security on the three tenant-scoped tables in
-- ai-governance-svc: ai_runs, automation_policies, automation_actions.
--
-- The other three tables in 000001 deliberately get NO policy, because
-- they carry no tenant_id and are correct that way:
--   * action_risk_classifications — the platform risk taxonomy, keyed by
--     action_type. Doc7 §G2 makes this one shared taxonomy, not a
--     per-tenant one.
--   * model_provider_registrations — doc7 §G6's provider/model registry,
--     approved once for the platform.
--   * policy_change_approvals — doc7 §G3 is explicit that policy changes
--     "alter governance truth across tenants and historical evaluation",
--     so approving one is platform administration. Adding a tenant column
--     here to make the schema look uniform would invent a boundary the
--     doc rules out; the control that belongs on it is authorization plus
--     doc7 §H2/§H3's self-approval block, both of which the handler
--     already enforces.
--
-- Why this migration is load-bearing: before the change that adds it, the
-- handlers took tenant_id from the caller's own request body or
-- ?tenant_id= query param. A policy keyed on app.tenant_id would have been
-- decorative while the application still let a caller name the tenant it
-- was writing to. The two have to land together.
--
-- tenant_id is UUID here (not VARCHAR as in the connector services), so
-- the setting needs an explicit ::uuid cast. NULLIF guards the empty
-- string: current_setting(..., true) returns '' when app.tenant_id was
-- never set, and ''::uuid raises invalid input syntax rather than simply
-- matching nothing — so without NULLIF a tenant-less query would error
-- instead of returning no rows. It must return no rows.
--
-- No platform-scope escape hatch on these three: every route that touches
-- them is an HTTP request carrying a verified tenant. There is no Kafka
-- consumer and no global chain here, unlike audit-event-store-svc.

ALTER TABLE ai_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON ai_runs
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE automation_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE automation_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON automation_policies
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE automation_actions ENABLE ROW LEVEL SECURITY;
ALTER TABLE automation_actions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON automation_actions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
