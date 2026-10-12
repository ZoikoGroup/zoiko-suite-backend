DROP TABLE IF EXISTS ncd_channel_decisions;
DROP TABLE IF EXISTS ncd_recipient_plans;
DROP TABLE IF EXISTS ncd_suppressions;
DROP FUNCTION IF EXISTS ncd_reject_suppression_mutation();
DROP TABLE IF EXISTS ncd_preference_changes;
DROP TABLE IF EXISTS ncd_preferences;
DROP FUNCTION IF EXISTS ncd_reject_evidence_mutation() CASCADE;
