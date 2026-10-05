-- 000020 down: removes the communication intent registry.
DROP TRIGGER IF EXISTS trg_guard_intent_version ON communication_intent_versions;
DROP TRIGGER IF EXISTS trg_guard_intent ON communication_intents;
DROP TABLE IF EXISTS communication_intent_versions;
DROP TABLE IF EXISTS communication_intents;
DROP FUNCTION IF EXISTS guard_intent_version();
DROP FUNCTION IF EXISTS guard_intent();
