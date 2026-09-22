-- Reverts the sweeper's RLS exemption and the index it runs on.
--
-- The expired_at backfill is NOT reverted, deliberately. Restoring the
-- observation-time values would mean writing back timestamps that were wrong
-- when they were written, and this service's own rule is that its history is
-- the evidence of what authority was held and when. A down migration may undo
-- a schema decision; it may not reintroduce a known misstatement into an
-- evidence table.
--
-- Run this only with the background sweeper stopped. With the exemption gone,
-- the sweep sees no rows outside the tenant it has installed -- which for a
-- background loop that installs none means it silently expires nothing.

DROP INDEX IF EXISTS idx_delegation_grants_active_effective_to;

DROP POLICY IF EXISTS outbox_tenant_isolation ON delegation_outbox;
CREATE POLICY outbox_tenant_isolation ON delegation_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON delegation_grants;
CREATE POLICY tenant_isolation_policy ON delegation_grants FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
