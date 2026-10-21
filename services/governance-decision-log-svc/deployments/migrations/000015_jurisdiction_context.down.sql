-- Migration: 000015_jurisdiction_context.down.sql

BEGIN;

ALTER TABLE governance_decisions DROP COLUMN IF EXISTS jurisdiction_context;

COMMIT;
