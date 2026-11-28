-- Migration 000028: let the outbox relay drain outbox_events under FORCE RLS.
--
-- 000022 put FORCE ROW LEVEL SECURITY on outbox_events with a policy that
-- admits only the request's own tenant (app.tenant_id). The relay workers
-- act for no tenant, so as the runtime role (zoiko_app, NOBYPASSRLS) their
-- poll saw no rows: from 19 Nov 2026, when 000022 was applied, no workflow
-- event reached Kafka and no workflow decision reached the governance ledger
-- (found 25 Nov 2026, DL-2). The relay now sets app.outbox_relay = 'true'
-- inside its own transaction, the pattern of policy-svc 000010 and
-- access-control-svc's event_outbox. NULLIF because a pooled connection that
-- has served a request reads an unset setting as ''.

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
