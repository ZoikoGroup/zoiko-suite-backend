-- Dropping the inbox discards its receipts, so a redelivered event would be
-- processed again. RaiseEvidenceConflict is itself idempotent per open
-- (statement line, payment) pair, which bounds the effect.
DROP TABLE IF EXISTS eventing_inbox;
DROP TABLE IF EXISTS eventing_repair_log;
DROP FUNCTION IF EXISTS eventing_repair_log_immutable();
