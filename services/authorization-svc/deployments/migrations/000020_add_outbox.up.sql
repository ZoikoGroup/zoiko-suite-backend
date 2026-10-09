-- 000020_add_outbox.up.sql
-- Transactional outbox for decision events (Governance Control Plane
-- invariant #10; Doc 03 §3.4).
--
-- authorization.granted / .denied and sod.violation.detected were written to
-- Kafka synchronously after the decision insert, and a write failure was logged
-- and dropped: a Kafka outage silently lost every denial and SoD violation the
-- platform's authorization engine made during it. They are now inserted here
-- in the SAME transaction as the access_decision_log row, and a relay publishes
-- them, so a recorded decision always has its events, at least once.
--
-- No RLS: the relay drains every tenant's rows, and nothing tenant-facing
-- reads this table. tenant_id is NULL for a tenantless decision, exactly as it
-- is on access_decision_log.
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type        TEXT        NOT NULL,
    message_key       TEXT        NOT NULL,
    message_value     JSONB       NOT NULL,
    tenant_id         UUID,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at      TIMESTAMPTZ,
    publish_attempts  INTEGER     NOT NULL DEFAULT 0,
    last_error        TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished
    ON outbox_events (created_at)
    WHERE published_at IS NULL;

-- Compose grants the app role through default privileges (init-db.sh); a
-- deployment using the dedicated app_authorization role needs it by name.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT, INSERT, UPDATE ON outbox_events TO app_authorization;
    END IF;
END
$$;
