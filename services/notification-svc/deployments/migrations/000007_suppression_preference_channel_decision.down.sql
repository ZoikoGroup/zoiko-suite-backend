-- Down migration for 000007_suppression_preference_channel_decision.up.sql

DROP POLICY IF EXISTS channel_decisions_tenant_isolation ON channel_decisions;
DROP TABLE IF EXISTS channel_decisions;

DROP POLICY IF EXISTS preferences_tenant_isolation ON preferences;
DROP TABLE IF EXISTS preferences;

DROP POLICY IF EXISTS suppressions_tenant_isolation ON suppressions;
DROP TABLE IF EXISTS suppressions;