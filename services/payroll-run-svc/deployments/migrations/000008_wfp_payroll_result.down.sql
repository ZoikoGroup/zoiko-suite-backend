DROP TABLE IF EXISTS wfp_payroll_correction_chains;
DROP TABLE IF EXISTS wfp_payroll_result_components;
DROP TABLE IF EXISTS wfp_payroll_result_workers;
DROP TABLE IF EXISTS wfp_payroll_results;
DROP FUNCTION IF EXISTS wfp_chain_guard();
DROP FUNCTION IF EXISTS wfp_result_insert_guard();
-- wfp_run_ref_guard() is restored to its Wave 1 definition by re-applying 000005.
