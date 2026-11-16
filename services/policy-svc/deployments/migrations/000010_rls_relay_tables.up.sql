-- Migration: 000010_rls_relay_tables.up.sql
--
-- Row-level security on the three tables the background workers drain:
-- the event outbox, the evidence outbox and the activation schedule. They
-- were the only tenant-bearing tables without it, so any connection could
-- read every tenant's event payloads (verifier step 6; the same finding as
-- workflow-svc's L-8).
--
-- A request transaction sets app.tenant_id and sees its own tenant's rows,
-- plus the platform rows (tenant_id IS NULL) that belong to global policies,
-- which every tenant can already read. The relay workers act for no tenant:
-- they set app.outbox_relay = 'true' inside their own transaction, the
-- pattern access-control-svc's event_outbox uses. NULLIF because a pooled
-- connection that has served a request reads an unset setting as ''.

ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON outbox
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );

-- Evidence always belongs to the evaluating tenant (tenant_id NOT NULL).
ALTER TABLE policy_evidence_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy_evidence_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON policy_evidence_outbox
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );

ALTER TABLE policy_activation_schedule ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy_activation_schedule FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON policy_activation_schedule
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
