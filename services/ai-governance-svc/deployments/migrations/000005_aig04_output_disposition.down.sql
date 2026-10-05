DROP TRIGGER IF EXISTS trigger_disposition_lifecycle ON output_dispositions;
DROP FUNCTION IF EXISTS aig04_enforce_disposition_lifecycle() CASCADE;
DROP TABLE IF EXISTS output_dispositions;
