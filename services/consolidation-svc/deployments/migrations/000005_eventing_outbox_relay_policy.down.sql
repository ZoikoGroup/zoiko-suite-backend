-- Migration: 000005_eventing_outbox_relay_policy.down.sql
--
-- Restores 000004's tenant-only policy (which locks the relay out under any
-- NOBYPASSRLS role — see the up migration).

DROP POLICY IF EXISTS eventing_outbox_tenant_isolation ON eventing_outbox;
CREATE POLICY tenant_isolation_policy ON eventing_outbox FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
