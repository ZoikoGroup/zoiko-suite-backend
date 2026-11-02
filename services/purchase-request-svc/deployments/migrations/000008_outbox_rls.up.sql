-- outbox_events joins the service's other tables under ENABLE + FORCE ROW
-- LEVEL SECURITY (it was the one tenant-scoped table without RLS).
-- Request-path inserts run inside the command's tenant transaction and are
-- confined to that tenant. The relay is not a request: it drains every
-- tenant's backlog, so it sets app.outbox_relay = 'true' and is admitted by
-- an explicit, auditable disjunct (same pattern as anomaly-detection-svc,
-- accounting-period-svc and access-control-svc).
-- NULLIF: a pooled connection keeps a custom GUC as '' after a
-- transaction-local set_config resets, so '' must mean "not set".
ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
