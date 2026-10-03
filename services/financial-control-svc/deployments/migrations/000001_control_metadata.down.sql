DROP TABLE IF EXISTS outbox_events, control_run_transitions, control_runs, materiality_policies,
    tolerance_policies, control_rule_versions, control_definitions CASCADE;
DROP FUNCTION IF EXISTS guard_rule_version();
DROP FUNCTION IF EXISTS reject_control_mutation();
