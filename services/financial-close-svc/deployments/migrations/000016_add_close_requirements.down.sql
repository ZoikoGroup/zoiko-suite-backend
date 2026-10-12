-- Migration: 000016_add_close_requirements.down.sql
-- Manual rollback only: every entity's close checklist, including its history
-- of removed requirements, is lost.

DROP TABLE IF EXISTS close_requirements;
DROP FUNCTION IF EXISTS guard_close_requirement_mutation();
