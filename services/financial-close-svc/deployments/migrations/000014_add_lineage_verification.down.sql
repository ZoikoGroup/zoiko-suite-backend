DROP INDEX IF EXISTS idx_lineage_edges_recorded_at;
DROP TABLE IF EXISTS lineage_quarantined_gaps;
DROP TABLE IF EXISTS lineage_trace_verifications;
DROP FUNCTION IF EXISTS reject_lineage_verification_mutation();
