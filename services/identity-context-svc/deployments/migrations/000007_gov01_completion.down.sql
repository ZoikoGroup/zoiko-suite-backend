-- Migration: 000007_gov01_completion.down.sql
--
-- Reverses 000007. Dropped in dependency order: session_contexts carries a
-- foreign key to support_contexts, so the column goes before the table.

-- Restore the plain tenant-only policies 000003 and 000005 installed, before
-- dropping the columns the sweep capability existed for.
DROP POLICY IF EXISTS tenant_isolation_policy ON access_decision_log;
CREATE POLICY tenant_isolation_policy ON access_decision_log
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS tenant_isolation_policy ON session_contexts;
CREATE POLICY tenant_isolation_policy ON session_contexts
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP INDEX IF EXISTS idx_access_decision_log_disposition_due;
ALTER TABLE access_decision_log
    DROP COLUMN IF EXISTS disposed_at,
    DROP COLUMN IF EXISTS disposition_due_at,
    DROP COLUMN IF EXISTS retention_class;

DROP INDEX IF EXISTS idx_session_contexts_tenant_issued;
DROP INDEX IF EXISTS idx_session_contexts_support;
DROP INDEX IF EXISTS idx_session_contexts_disposition_due;

ALTER TABLE session_contexts
    DROP CONSTRAINT IF EXISTS support_context_fk;
ALTER TABLE session_contexts
    DROP CONSTRAINT IF EXISTS session_contexts_environment_check;

ALTER TABLE session_contexts
    DROP COLUMN IF EXISTS disposed_at,
    DROP COLUMN IF EXISTS disposition_due_at,
    DROP COLUMN IF EXISTS retention_class,
    DROP COLUMN IF EXISTS support_context_id,
    DROP COLUMN IF EXISTS evidence_id,
    DROP COLUMN IF EXISTS environment,
    DROP COLUMN IF EXISTS ingress_source;

DROP TABLE IF EXISTS legal_hold_projection;
DROP TABLE IF EXISTS tenant_ingress_bindings;
DROP TABLE IF EXISTS support_contexts;
DROP TABLE IF EXISTS event_outbox;
