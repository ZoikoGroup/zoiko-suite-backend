-- ZS-SVC-Z-001 DQC-02 / DQC-01 (docs/architecture/DQC_GAP_MATRIX.md, step F2).
--
-- A run used to pin a rule-set version, which has no severity, no dimension and no notion of which
-- rules certification requires. A run can now pin an exact, PUBLISHED contract version instead.
--
--   dq_runs.contract_id   the exact contract version the run was judged against. Exactly one of
--                         ruleset_version_id / contract_id is set: a run is never judged against
--                         both, and never against neither.
--   dq_results.severity / dimension / mandatory
--                         the rule's severity, quality dimension and certification-profile
--                         membership AS THEY WERE in that contract version, frozen on the result
--                         (results are append-only, so they cannot be edited retroactively).
--                         All three are set together or not at all; results of rule-set runs have none.
--
-- Nothing is backfilled: existing runs and results keep their rule-set binding and carry no severity,
-- because no severity was ever recorded for them and none can be honestly invented.

ALTER TABLE dq_runs ALTER COLUMN ruleset_version_id DROP NOT NULL;

ALTER TABLE dq_runs
    ADD COLUMN IF NOT EXISTS contract_id TEXT REFERENCES dqc_contract_versions(contract_id);

ALTER TABLE dq_runs DROP CONSTRAINT IF EXISTS dq_runs_exactly_one_rule_source;
ALTER TABLE dq_runs
    ADD CONSTRAINT dq_runs_exactly_one_rule_source
        CHECK ((ruleset_version_id IS NULL) <> (contract_id IS NULL));

CREATE INDEX IF NOT EXISTS idx_dq_runs_contract ON dq_runs(tenant_id, contract_id) WHERE contract_id IS NOT NULL;

ALTER TABLE dq_results
    ADD COLUMN IF NOT EXISTS severity  VARCHAR(16),
    ADD COLUMN IF NOT EXISTS dimension VARCHAR(32),
    ADD COLUMN IF NOT EXISTS mandatory BOOLEAN;

ALTER TABLE dq_results DROP CONSTRAINT IF EXISTS dq_results_severity_values;
ALTER TABLE dq_results DROP CONSTRAINT IF EXISTS dq_results_contract_fields_together;
ALTER TABLE dq_results
    ADD CONSTRAINT dq_results_severity_values
        CHECK (severity IS NULL OR severity IN ('INFO', 'WARNING', 'ERROR', 'BLOCKER')),
    ADD CONSTRAINT dq_results_contract_fields_together
        CHECK ((severity IS NULL) = (dimension IS NULL) AND (severity IS NULL) = (mandatory IS NULL));
-- A BLOCKER is never optional for certification.
ALTER TABLE dq_results DROP CONSTRAINT IF EXISTS dq_results_blocker_is_mandatory;
ALTER TABLE dq_results
    ADD CONSTRAINT dq_results_blocker_is_mandatory CHECK (severity IS DISTINCT FROM 'BLOCKER' OR mandatory);
