-- Down migration for 000007_refused_escalations.up.sql

DROP POLICY IF EXISTS refused_escalations_tenant_isolation ON refused_escalations;
DROP TABLE IF EXISTS refused_escalations;