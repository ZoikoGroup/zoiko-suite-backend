DROP TRIGGER IF EXISTS authz_config_history_events ON authz_config_history;
DROP TRIGGER IF EXISTS da_iam_events ON delegated_authorities;
DROP TRIGGER IF EXISTS pra_iam_events ON principal_role_assignments;
DROP FUNCTION IF EXISTS authz_config_events();
DROP FUNCTION IF EXISTS authz_delegation_events();
DROP FUNCTION IF EXISTS authz_assignment_events();
DROP FUNCTION IF EXISTS authz_emit_event(TEXT, TEXT, UUID, TEXT, JSONB);
