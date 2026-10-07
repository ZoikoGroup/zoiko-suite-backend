DROP TRIGGER IF EXISTS trg_reject_form_submission_route_mutation ON form_submission_routes;
DROP FUNCTION IF EXISTS reject_form_submission_route_mutation();
DROP TRIGGER IF EXISTS trg_reject_form_submission_mutation ON form_submissions;
DROP FUNCTION IF EXISTS reject_form_submission_mutation();
DROP TRIGGER IF EXISTS trg_reject_form_definition_mutation ON form_definitions;
DROP FUNCTION IF EXISTS reject_form_definition_mutation();
DROP TABLE IF EXISTS form_submission_routes;
DROP TABLE IF EXISTS form_submissions;
DROP TABLE IF EXISTS form_definitions;
