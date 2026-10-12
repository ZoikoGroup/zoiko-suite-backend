-- 000024: open exceptions join the §13.1 backlog (ZS-SVC-Y-001 §13.1, §13.2).
--
-- ncd_exceptions is the human path of the plane: UNKNOWN past its deadline,
-- missing callbacks, blocked fallbacks, misdelivery incidents. Every other
-- backlog the worker gauges (jobs, attempts, notices) has a SELECT-only
-- platform-scope policy; this table had none, so under FORCE ROW LEVEL
-- SECURITY the cross-tenant snapshot saw zero rows and an operator could see
-- open exceptions only tenant by tenant, never as an alert.
--
-- Same shape as ncd_jobs_platform_read (000018): SELECT only, gated on the
-- app.platform_scope flag the worker installs, and the query that uses it
-- projects counts and ages only — no id, tenant or address leaves the database.
DROP POLICY IF EXISTS ncd_exceptions_platform_read ON ncd_exceptions;
CREATE POLICY ncd_exceptions_platform_read ON ncd_exceptions FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.platform_scope', true), ''), 'false') = 'true');
