-- 000021_intent_binding.up.sql
-- ZS-SVC-Y-001 NCD-01 (4.2 template_set, 9.1 Communication -> intent_version) and
-- INV-04 ("every dispatched message pins exact intent"), Wave 2 slice 3 continued.
--
-- 1. A template DEFINITION can be bound to a communication intent, at creation and for
--    good: the intent says what the wording is FOR (purpose, channels, the variables it
--    may be given), and a template that could be rebound later could be moved under a
--    more permissive intent after it was reviewed. The bound intent must belong to the
--    same tenant, checked under the caller's row-level security so an intent the
--    caller cannot see is a mismatch.
--
-- 2. A NOTIFICATION records the exact intent VERSION it was sent under, fixed at
--    creation: a message can be reconstructed against the contract that governed it,
--    not whatever the intent says today.
--
-- Both columns are nullable: every existing template and notification, and every send
-- that uses an unbound template or free text, has no intent, exactly as before.

ALTER TABLE template_definitions
    ADD COLUMN IF NOT EXISTS intent_id UUID REFERENCES communication_intents (intent_id);

ALTER TABLE notifications
    ADD COLUMN IF NOT EXISTS intent_version_id UUID REFERENCES communication_intent_versions (version_id);

CREATE INDEX IF NOT EXISTS idx_template_definitions_intent ON template_definitions (intent_id) WHERE intent_id IS NOT NULL;

CREATE OR REPLACE FUNCTION guard_template_intent_binding() RETURNS trigger AS $$
DECLARE
    intent_tenant TEXT;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.intent_id IS DISTINCT FROM OLD.intent_id THEN
        RAISE EXCEPTION 'a template''s intent binding is fixed when the template is created' USING ERRCODE = '23514';
    END IF;
    IF NEW.intent_id IS NOT NULL THEN
        SELECT tenant_id INTO intent_tenant FROM communication_intents WHERE intent_id = NEW.intent_id;
        IF intent_tenant IS DISTINCT FROM NEW.tenant_id THEN
            RAISE EXCEPTION 'a template can only be bound to an intent of the same tenant' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_template_intent_binding ON template_definitions;
CREATE TRIGGER trg_template_intent_binding
    BEFORE INSERT OR UPDATE OF intent_id ON template_definitions
    FOR EACH ROW EXECUTE FUNCTION guard_template_intent_binding();

CREATE OR REPLACE FUNCTION freeze_notification_intent_version() RETURNS trigger AS $$
BEGIN
    IF NEW.intent_version_id IS DISTINCT FROM OLD.intent_version_id THEN
        RAISE EXCEPTION 'the intent version a notification was sent under is fixed when it is created' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_notifications_freeze_intent_version ON notifications;
CREATE TRIGGER trg_notifications_freeze_intent_version
    BEFORE UPDATE OF intent_version_id ON notifications
    FOR EACH ROW EXECUTE FUNCTION freeze_notification_intent_version();
