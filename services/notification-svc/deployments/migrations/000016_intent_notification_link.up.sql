-- 000016_intent_notification_link.up.sql
-- ZS-SVC-Y-001 INV-02, Wave 1 step 2 of notification-ncd-communication-identity-plan.md.
--
-- notification-svc has two send paths with two identities: the direct path
-- (notifications.notification_id, which the outbox events already call the
-- communication_id) and the ledger pipeline (message_intents.message_intent_id).
-- One logical communication needs one identity. This migration adds ONLY the link
-- between the two; nothing reads or writes it yet, so behaviour is unchanged.
-- Later steps make the ledger path create the notifications row and link it.
--
--   message_intents.notification_id  -> the communication this intent became
--   notifications.message_intent_id  -> the intent that produced this notification
--
-- Both are nullable (every existing row, and every direct send, has no intent),
-- one-to-one (partial unique indexes), and set together by the store in one
-- transaction.
--
-- ON DELETE SET NULL: housekeeping deletes stale message_intents (000009), and a
-- notification is the durable record, so an intent being purged must not be
-- blocked by, or take down, the notification that points at it.
--
-- Same tenant, always. A foreign key cannot say that, so a trigger does. It runs
-- under the caller's row-level security: a row the caller cannot see is treated as
-- a mismatch, so a cross-tenant link fails closed. Once set, a link cannot be
-- repointed at a different row (it may only be cleared, which the foreign key's
-- own SET NULL needs).

ALTER TABLE message_intents
    ADD COLUMN IF NOT EXISTS notification_id UUID
        REFERENCES notifications (notification_id) ON DELETE SET NULL;

ALTER TABLE notifications
    ADD COLUMN IF NOT EXISTS message_intent_id UUID
        REFERENCES message_intents (message_intent_id) ON DELETE SET NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_message_intents_notification
    ON message_intents (notification_id) WHERE notification_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_notifications_message_intent
    ON notifications (message_intent_id) WHERE message_intent_id IS NOT NULL;

CREATE OR REPLACE FUNCTION guard_intent_notification_link() RETURNS trigger AS $$
DECLARE
    other_tenant     TEXT;
    old_link         UUID;
    new_link         UUID;
    counterpart_link UUID;
    own_id           UUID;
BEGIN
    IF TG_TABLE_NAME = 'message_intents' THEN
        new_link := NEW.notification_id;
        IF TG_OP = 'UPDATE' THEN old_link := OLD.notification_id; END IF;
    ELSE
        new_link := NEW.message_intent_id;
        IF TG_OP = 'UPDATE' THEN old_link := OLD.message_intent_id; END IF;
    END IF;

    IF new_link IS NULL THEN
        RETURN NEW; -- clearing a link, or never linked
    END IF;

    IF old_link IS NOT NULL AND old_link IS DISTINCT FROM new_link THEN
        RAISE EXCEPTION 'an intent/notification link cannot be repointed once set' USING ERRCODE = '23514';
    END IF;

    IF TG_TABLE_NAME = 'message_intents' THEN
        SELECT tenant_id, message_intent_id INTO other_tenant, counterpart_link FROM notifications WHERE notification_id = new_link;
        own_id := NEW.message_intent_id;
    ELSE
        SELECT tenant_id, notification_id INTO other_tenant, counterpart_link FROM message_intents WHERE message_intent_id = new_link;
        own_id := NEW.notification_id;
    END IF;

    IF other_tenant IS DISTINCT FROM NEW.tenant_id THEN
        RAISE EXCEPTION 'an intent can only be linked to a notification of the same tenant' USING ERRCODE = '23514';
    END IF;

    -- The two sides must agree. The counterpart may still be unlinked (the store
    -- sets one side and then the other in one transaction) but may not already
    -- point somewhere else: otherwise one side could claim a row whose own pointer
    -- names a different partner, and the "one communication" would be two.
    IF counterpart_link IS NOT NULL AND counterpart_link IS DISTINCT FROM own_id THEN
        RAISE EXCEPTION 'the counterpart is already linked to a different row' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_message_intents_link_guard ON message_intents;
CREATE TRIGGER trg_message_intents_link_guard
    BEFORE INSERT OR UPDATE OF notification_id ON message_intents
    FOR EACH ROW EXECUTE FUNCTION guard_intent_notification_link();

DROP TRIGGER IF EXISTS trg_notifications_link_guard ON notifications;
CREATE TRIGGER trg_notifications_link_guard
    BEFORE INSERT OR UPDATE OF message_intent_id ON notifications
    FOR EACH ROW EXECUTE FUNCTION guard_intent_notification_link();
