-- 000019_notification_communication_class.up.sql
-- ZS-SVC-Y-001 INV-06, INV-07 and the identity plan step 5 (one policy gate).
--
-- The direct send path had no notion of what KIND of message it was sending, so the
-- guard in front of it had to assume one (transactional). Purpose is explicit in
-- the standard: "Purpose classification is explicit and cannot be inferred solely
-- from channel or audience" (INV-06), and marketing permission is distinct from
-- service necessity (INV-07).
--
-- communication_class uses the classes the ledger pipeline and its precedence engine
-- already use (S0 security, T0 transactional, A1 operational, L1 lifecycle,
-- M1 marketing), so both send paths are judged by the same rules. NULL means the
-- sender stated no class; it is treated as T0 and the row says so by being NULL.
--
-- The class decides which suppressions and preferences may block a message, so it
-- is part of the permission semantics. It is fixed when the row is created: a
-- retry, a resend or a clever UPDATE must not be able to reclassify a message
-- after the fact (for example from marketing to transactional to slip past an
-- opt-out).

ALTER TABLE notifications
    ADD COLUMN IF NOT EXISTS communication_class VARCHAR(10);

ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS ck_notifications_communication_class,
    ADD CONSTRAINT ck_notifications_communication_class
        CHECK (communication_class IS NULL OR communication_class IN ('S0', 'T0', 'A1', 'L1', 'M1'));

CREATE OR REPLACE FUNCTION freeze_notification_communication_class() RETURNS trigger AS $$
BEGIN
    IF NEW.communication_class IS DISTINCT FROM OLD.communication_class THEN
        RAISE EXCEPTION 'a notification''s communication class is fixed when it is created' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_notifications_freeze_class ON notifications;
CREATE TRIGGER trg_notifications_freeze_class
    BEFORE UPDATE OF communication_class ON notifications
    FOR EACH ROW EXECUTE FUNCTION freeze_notification_communication_class();
