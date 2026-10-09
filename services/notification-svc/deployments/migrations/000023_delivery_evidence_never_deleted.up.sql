-- 000023: delivery evidence is never deleted (ZS-SVC-Y-001 §8.4, §9.2, INV-28).
--
-- The housekeeping worker used to DELETE concluded message_intents older than
-- 90 days, and ON DELETE CASCADE took their message_renders, delivery_attempts
-- and delivery_events with them — the record of what exact content went to
-- which address through which provider, destroyed on a timer and without any
-- legal-hold check. That step is gone; this makes the database refuse it, so
-- no later code path can quietly bring it back.
--
-- Retention of communication evidence belongs to DRC (§1.4, §9.3). When DRC
-- exists and declares a disposition, the removal will be a governed migration
-- of its own, not a DELETE from this service. DROP / TRUNCATE by an operator
-- (test resets) are not row deletes and are unaffected.

CREATE OR REPLACE FUNCTION notification_reject_evidence_delete() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% rows are delivery evidence and are never deleted (ZS-SVC-Y-001 INV-28); retention is DRC''s', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['message_intents', 'message_renders', 'delivery_attempts', 'delivery_events',
                             'notifications', 'notification_delivery_attempts']
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_reject_evidence_delete ON %I', t);
        EXECUTE format('CREATE TRIGGER trg_reject_evidence_delete BEFORE DELETE ON %I
                        FOR EACH ROW EXECUTE FUNCTION notification_reject_evidence_delete()', t);
    END LOOP;
END $$;
