-- Migration: 000017_add_close_reliance_manifest.down.sql
-- Manual rollback only: every close's record of what it relied on is lost.

ALTER TABLE close_evidences DROP CONSTRAINT IF EXISTS close_evidences_reliance_complete;
ALTER TABLE close_evidences DROP COLUMN IF EXISTS reliance_signature;
ALTER TABLE close_evidences DROP COLUMN IF EXISTS reliance_hash;
ALTER TABLE close_evidences DROP COLUMN IF EXISTS reliance_manifest;
