-- Migration: 000009_rls_force_with_check.up.sql
--
-- 000001 enabled row-level security on reconciliation_jobs and
-- reconciliation_unmatched_items but did not FORCE it, so the table owner
-- was exempt, and its policies stated only USING. FORCE binds every
-- non-superuser role (the owner included); the explicit WITH CHECK states
-- that a row may only be written for the tenant in app.tenant_id.
--
-- eventing_outbox (000002, a verbatim copy of the shared library schema) had
-- no row-level security at all. It gets the same policy migration-integrity-
-- svc uses for the same library table: tenant-confined for request-path
-- writes, with the explicit relay escape hatch (the shared relay installs
-- app.outbox_relay = 'true' in its own transaction to drain every tenant;
-- a tenant-only policy would hide every row from it). 000002 itself is not
-- edited, so TestEventingOutboxMigrationMatchesLibrary still holds.
-- Nothing else changes.

ALTER TABLE reconciliation_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_jobs FORCE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_unmatched_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_unmatched_items FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS reconciliation_jobs_tenant_isolation ON reconciliation_jobs;
CREATE POLICY reconciliation_jobs_tenant_isolation ON reconciliation_jobs FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS reconciliation_unmatched_items_tenant_isolation ON reconciliation_unmatched_items;
CREATE POLICY reconciliation_unmatched_items_tenant_isolation ON reconciliation_unmatched_items FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE eventing_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_outbox FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS eventing_outbox_tenant_isolation ON eventing_outbox;
CREATE POLICY eventing_outbox_tenant_isolation ON eventing_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
