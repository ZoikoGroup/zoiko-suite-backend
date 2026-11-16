-- Migration: 000017_sealer_read_and_correlation_index.down.sql

BEGIN;

DROP POLICY IF EXISTS evidence_sealer_read ON evidence_packages;
DROP POLICY IF EXISTS evidence_sealer_read ON governance_decisions;
DROP INDEX IF EXISTS idx_governance_decisions_tenant_correlation;

COMMIT;
