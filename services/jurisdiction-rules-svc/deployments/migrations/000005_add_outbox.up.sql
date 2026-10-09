-- 000005_add_outbox.up.sql
-- Transactional outbox for reliable event publishing.
-- Events are written to this table in the same transaction as the domain change,
-- then a background worker publishes them to Kafka. This guarantees at-least-once
-- delivery and survives broker outages (Event Catalogue §6, GOV §2 invariant #10).

CREATE TABLE IF NOT EXISTS event_outbox (
    outbox_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type        VARCHAR(255) NOT NULL,
    event_payload     JSONB NOT NULL,
    topic             VARCHAR(255) NOT NULL,
    partition_key     VARCHAR(255),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at      TIMESTAMPTZ,
    publish_attempts  INT NOT NULL DEFAULT 0,
    last_error        TEXT
);

CREATE INDEX IF NOT EXISTS idx_event_outbox_pending
    ON event_outbox (created_at)
    WHERE published_at IS NULL;

-- Grant minimal permissions to the app role
-- The runtime role is app_jurisdiction_rules (create-app-roles.sh). This used to
-- name jurisdiction_rules_app, which nothing creates, so the migration failed
-- and a fresh volume could not initialise. On a fresh volume roles are created
-- after migrations and default privileges cover this table; hence the guard.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT ON event_outbox TO app_jurisdiction_rules;
    END IF;
END $$;