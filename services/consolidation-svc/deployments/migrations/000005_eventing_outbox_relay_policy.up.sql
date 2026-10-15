-- Migration: 000005_eventing_outbox_relay_policy.up.sql
--
-- Fixes 000004's RLS policy on eventing_outbox, which admitted only
-- tenant_id = app.tenant_id. The outbox relay drains every tenant's backlog
-- from one background loop and installs app.outbox_relay instead of a tenant
-- (eventing/outbox inRelayTx), so under FORCE ROW LEVEL SECURITY any role
-- without BYPASSRLS — zoiko_app included — saw no rows and the relay never
-- published anything. Tenant isolation is kept; the relay is admitted by an
-- explicit, auditable disjunct (access-control-svc's pattern). NULLIF because a
-- transaction-local custom GUC reads as '' (not NULL) on a pooled connection
-- that has already served a request.

DROP POLICY IF EXISTS tenant_isolation_policy ON eventing_outbox;
CREATE POLICY eventing_outbox_tenant_isolation ON eventing_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
