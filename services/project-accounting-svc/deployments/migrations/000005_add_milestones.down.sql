DROP TABLE IF EXISTS project_recognition_run_milestones CASCADE;
DROP TABLE IF EXISTS project_milestones CASCADE;
DROP FUNCTION IF EXISTS reject_run_milestone_evidence_mutation();
DROP FUNCTION IF EXISTS reject_milestone_economic_mutation();
ALTER TABLE project_financial_profiles DROP CONSTRAINT chk_recognition_method;
ALTER TABLE project_financial_profiles ADD CONSTRAINT chk_recognition_method CHECK (
    recognition_method IN ('PERCENTAGE_OF_COMPLETION', 'COMPLETED_CONTRACT', 'TIME_AND_MATERIALS'));
