-- Migration: 000002_outbox_events_relay_policy.up.sql
--
-- Fixes 000001's RLS policy on outbox_events, which admitted only
-- tenant_id = app.tenant_id. The outbox Relay (internal/outbox/outbox.go)
-- polls unpublished rows across every tenant from one background loop and
-- never calls set_config at all -- it sets neither app.tenant_id nor any
-- relay marker. Under FORCE ROW LEVEL SECURITY, any role without BYPASSRLS
-- (zoiko_app included) would see zero rows for that policy and the relay
-- would silently never publish anything in production.
--
-- Same bug class and fix as consolidation-svc's 000005 and
-- financial-close-svc's 000020: keep tenant isolation, add an explicit,
-- auditable app.outbox_relay disjunct (access-control-svc's pattern). NULLIF
-- because a transaction-local custom GUC reads as '' (not NULL) on a pooled
-- connection that has already served a request.

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
