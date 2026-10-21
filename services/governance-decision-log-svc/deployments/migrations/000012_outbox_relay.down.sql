-- Migration: 000012_outbox_relay.down.sql
-- Restores the 000009 tenant-only policy. The relay cannot drain the outbox
-- under it as the runtime role.

BEGIN;

DROP POLICY IF EXISTS tenant_isolation_policy ON outbox;
CREATE POLICY tenant_isolation_policy ON outbox
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE outbox
    ALTER COLUMN payload TYPE JSONB USING convert_from(payload, 'UTF8')::jsonb;
ALTER TABLE outbox DROP COLUMN IF EXISTS aggregate_id;

COMMIT;
