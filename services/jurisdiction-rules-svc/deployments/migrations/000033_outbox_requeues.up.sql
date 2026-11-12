-- 000033_outbox_requeues.up.sql
--
-- Governance audit J-16: an outbox event the broker refused MaxOutboxAttempts (10)
-- times is parked, so the events behind it keep flowing; until now nothing could
-- put it back except a hand-written UPDATE. POST /v1/admin/outbox/{id}/requeue
-- resets its attempts, and every requeue is kept here, append-only: who, why, on
-- what evidence, and what the event had failed with.
--
-- The relay claims rows with FOR UPDATE SKIP LOCKED (J-15) and marks them, so the
-- runtime role needs UPDATE on event_outbox, which 000005 never granted (the dev
-- stack's create-app-roles.sh grants it to every table; a migration-only
-- deployment did not have it).

CREATE TABLE IF NOT EXISTS outbox_requeues (
    requeue_id        UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    outbox_id         UUID         NOT NULL REFERENCES event_outbox(outbox_id),
    event_type        VARCHAR(255) NOT NULL,
    attempts_before   INT          NOT NULL,
    last_error        TEXT,
    reason            TEXT         NOT NULL CHECK (length(btrim(reason)) > 0),
    evidence_ref      TEXT,
    requested_by      TEXT         NOT NULL,
    requested_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_outbox_requeues_outbox ON outbox_requeues (outbox_id, requested_at);

-- Its own guard function, so this migration and the pack registries' (000005,
-- 000007) can each be reverted without the other.
CREATE OR REPLACE FUNCTION jur_outbox_requeue_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'outbox_requeues is append-only: % refused', TG_OP USING ERRCODE = 'check_violation';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_outbox_requeues_append_only ON outbox_requeues;
CREATE TRIGGER trg_outbox_requeues_append_only BEFORE UPDATE OR DELETE ON outbox_requeues
    FOR EACH ROW EXECUTE FUNCTION jur_outbox_requeue_immutable();

-- The parked list reads unpublished rows at or above the attempt limit.
CREATE INDEX IF NOT EXISTS idx_event_outbox_parked
    ON event_outbox (created_at)
    WHERE published_at IS NULL AND publish_attempts >= 10;

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT UPDATE ON event_outbox TO app_jurisdiction_rules;
        GRANT SELECT, INSERT ON outbox_requeues TO app_jurisdiction_rules;
    END IF;
END $$;
