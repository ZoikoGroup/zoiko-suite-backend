DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['message_intents', 'message_renders', 'delivery_attempts', 'delivery_events',
                             'notifications', 'notification_delivery_attempts']
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_reject_evidence_delete ON %I', t);
    END LOOP;
END $$;
DROP FUNCTION IF EXISTS notification_reject_evidence_delete();
