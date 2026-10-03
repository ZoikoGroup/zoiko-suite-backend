-- Rollback for AIG-01 use-case registry. Leaves the doc7-based tables
-- from 000001/000002 intact.

DROP TRIGGER IF EXISTS trigger_use_case_lifecycle ON ai_use_cases;
DROP FUNCTION IF EXISTS aig01_enforce_use_case_lifecycle();

DROP TRIGGER IF EXISTS trigger_assessment_immutability ON ai_impact_assessments;
DROP FUNCTION IF EXISTS aig01_enforce_assessment_immutability();

DROP TABLE IF EXISTS ai_impact_assessments CASCADE;
DROP TABLE IF EXISTS ai_use_cases CASCADE;
