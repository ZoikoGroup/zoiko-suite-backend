-- Migration: 000015_add_break_glass_and_support_sessions.down.sql
--
-- Drops the break_glass_sessions and support_sessions tables.

BEGIN;

DROP TABLE IF EXISTS break_glass_sessions;
DROP TABLE IF EXISTS support_sessions;

COMMIT;
