-- Migration: 000014_add_privileged_sessions.down.sql
--
-- Drops the privileged_sessions table.

BEGIN;

DROP TABLE IF EXISTS privileged_sessions;

COMMIT;
