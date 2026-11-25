-- ZS-SVC-Z-001 DQC-02 (docs/architecture/DQC_GAP_MATRIX.md, step G): a validation run can name the observation
-- its population was taken from. The link is only meaningful for a run judged against a contract (an observation
-- is of a registered asset), and the service requires the run's population to BE what was observed: same row
-- count and the observation's evidence hash as the population content hash.
--
-- Nullable: existing runs name no observation and none is invented for them.

ALTER TABLE dq_runs ADD COLUMN IF NOT EXISTS observation_id TEXT REFERENCES dq_observations(observation_run_id);
ALTER TABLE dq_runs DROP CONSTRAINT IF EXISTS dq_runs_observation_needs_contract;
ALTER TABLE dq_runs ADD CONSTRAINT dq_runs_observation_needs_contract
    CHECK (observation_id IS NULL OR contract_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_dq_runs_observation ON dq_runs(tenant_id, observation_id) WHERE observation_id IS NOT NULL;
