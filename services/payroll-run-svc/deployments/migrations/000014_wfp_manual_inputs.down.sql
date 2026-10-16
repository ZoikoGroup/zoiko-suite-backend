DROP TABLE IF EXISTS wfp_manual_inputs;
DROP FUNCTION IF EXISTS wfp_manual_guard();
-- Restoring the narrower checks fails loudly if a MANUAL mapping or line exists: that evidence is never discarded.
ALTER TABLE wfp_input_code_mappings DROP CONSTRAINT IF EXISTS wfp_map_kind_check;
ALTER TABLE wfp_input_code_mappings ADD CONSTRAINT wfp_map_kind_check CHECK (source_kind IN
    ('BASE_SALARY', 'COMP_COMPONENT', 'COMP_BONUS', 'BENEFIT_EMPLOYEE', 'BENEFIT_EMPLOYER'));
ALTER TABLE wfp_payroll_input_lines DROP CONSTRAINT IF EXISTS wfp_in_line_kind_check;
ALTER TABLE wfp_payroll_input_lines ADD CONSTRAINT wfp_in_line_kind_check CHECK (source_kind IN
    ('BASE_SALARY', 'COMP_COMPONENT', 'COMP_BONUS', 'BENEFIT_EMPLOYEE', 'BENEFIT_EMPLOYER'));
