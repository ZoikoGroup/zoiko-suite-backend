-- Migration: 000016_evidence_ledger.down.sql
--
-- Revert G-07 Evidence ledger.

BEGIN;

-- Drop the functions
DROP FUNCTION IF EXISTS seal_package(TEXT, TEXT, TEXT, TEXT);
DROP FUNCTION IF EXISTS compute_curr_hash(TEXT, TEXT, TIMESTAMPTZ, TEXT);

-- Drop the sealed_packages table
DROP TABLE IF EXISTS sealed_packages;

-- Remove hash chain columns
ALTER TABLE governance_decisions
    DROP COLUMN IF EXISTS prev_hash,
    DROP COLUMN IF EXISTS curr_hash;

COMMIT;