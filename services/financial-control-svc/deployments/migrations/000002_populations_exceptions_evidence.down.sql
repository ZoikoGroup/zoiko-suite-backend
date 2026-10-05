DROP TABLE IF EXISTS command_idempotency, evidence_packages, exception_transitions, control_exceptions,
    match_results, population_records, population_snapshots CASCADE;
DROP FUNCTION IF EXISTS guard_control_exception();
