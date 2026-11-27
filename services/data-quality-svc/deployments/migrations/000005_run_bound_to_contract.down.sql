-- Refused while any run is bound to a contract: removing contract_id would orphan its rules.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM dq_runs WHERE contract_id IS NOT NULL) THEN
        RAISE EXCEPTION '000005 down refused: runs are bound to contract versions';
    END IF;
END
$$;
ALTER TABLE dq_results DROP CONSTRAINT IF EXISTS dq_results_blocker_is_mandatory;
ALTER TABLE dq_results DROP CONSTRAINT IF EXISTS dq_results_contract_fields_together;
ALTER TABLE dq_results DROP CONSTRAINT IF EXISTS dq_results_severity_values;
ALTER TABLE dq_results DROP COLUMN IF EXISTS mandatory, DROP COLUMN IF EXISTS dimension, DROP COLUMN IF EXISTS severity;
ALTER TABLE dq_runs DROP CONSTRAINT IF EXISTS dq_runs_exactly_one_rule_source;
DROP INDEX IF EXISTS idx_dq_runs_contract;
ALTER TABLE dq_runs DROP COLUMN IF EXISTS contract_id;
ALTER TABLE dq_runs ALTER COLUMN ruleset_version_id SET NOT NULL;
