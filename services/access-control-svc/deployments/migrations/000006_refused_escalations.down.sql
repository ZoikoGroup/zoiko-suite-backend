-- Migration 000006 down: Refused escalations table
DROP INDEX IF EXISTS idx_refused_escalations_principal;
DROP INDEX IF EXISTS idx_refused_escalations_reason;
DROP INDEX IF EXISTS idx_refused_escalations_correlation;
DROP INDEX IF EXISTS idx_refused_escalations_tenant_time;
DROP TABLE IF EXISTS refused_escalations;