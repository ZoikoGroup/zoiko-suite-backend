-- Rollback for AIG-02 model release registry. Leaves
-- model_provider_registrations (000001) intact.

DROP TRIGGER IF EXISTS trigger_release_lifecycle ON ai_model_releases;
DROP FUNCTION IF EXISTS aig02_enforce_release_lifecycle();

DROP TABLE IF EXISTS ai_model_releases CASCADE;
