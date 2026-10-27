-- Migration: 000012_outbox_relay.up.sql
--
-- Makes the outbox (000009) drainable by the relay.
--
-- 000009 put the outbox under FORCE row-level security keyed on app.tenant_id,
-- which is right for the request path: a write files its event under the
-- tenant it was made in. But the relay is not acting for any one tenant -- it
-- drains every tenant's events -- and it read the table with no tenant set. As
-- the runtime role (NOBYPASSRLS, the one compose connects as) that SELECT
-- matched no rows at all, so no event was ever published and nothing said so.
--
-- The relay now marks its own transaction with app.outbox_relay = 'on', and
-- the policy admits that marker. It is a transaction-local setting the relay
-- sets for itself; the request path never sets it.
--
-- aggregate_id carries the decision_id so the relay can key the Kafka message
-- by the aggregate (Event Catalogue §5) rather than by correlation_id.

BEGIN;

ALTER TABLE outbox
    ADD COLUMN IF NOT EXISTS aggregate_id VARCHAR(64);

-- The envelope is published byte for byte as it was built. As JSONB it was
-- re-serialised by Postgres (keys re-ordered, whitespace changed), so the bytes
-- published were not the bytes the envelope's payload_hash was computed over,
-- and every consumer verifying the hash would have rejected every event.
ALTER TABLE outbox
    ALTER COLUMN payload TYPE BYTEA USING convert_to(payload::text, 'UTF8');

DROP POLICY IF EXISTS tenant_isolation_policy ON outbox;
CREATE POLICY tenant_isolation_policy ON outbox
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.outbox_relay', true) = 'on')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)
                OR current_setting('app.outbox_relay', true) = 'on');

COMMIT;
