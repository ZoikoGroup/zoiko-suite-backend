-- Migration: 000006_device_trust_score_nullable.down.sql
--
-- Reinstating NOT NULL requires a value for rows that recorded the absence of
-- one. 0 is the only value the column can take, and it is the wrong one — it
-- reads as "assessed, maximally untrusted". That misreading is exactly what the
-- up migration existed to remove, and this restores it.

UPDATE session_contexts SET device_trust_score = 0 WHERE device_trust_score IS NULL;

ALTER TABLE session_contexts
    ALTER COLUMN device_trust_score SET DEFAULT 0,
    ALTER COLUMN device_trust_score SET NOT NULL;
