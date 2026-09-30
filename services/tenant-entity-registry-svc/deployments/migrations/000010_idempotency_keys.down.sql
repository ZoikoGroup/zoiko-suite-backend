-- 000010_idempotency_keys.down.sql
--
-- Reverses 000010. Recorded command responses are dropped with the table;
-- a retry after this runs the command again, as it did before 000010.

DROP TABLE IF EXISTS idempotency_keys;
