-- Refused while any run names an observation: dropping the column would discard that link.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM dq_runs WHERE observation_id IS NOT NULL) THEN
        RAISE EXCEPTION '000008 down refused: runs are linked to observations';
    END IF;
END
$$;
DROP INDEX IF EXISTS idx_dq_runs_observation;
ALTER TABLE dq_runs DROP CONSTRAINT IF EXISTS dq_runs_observation_needs_contract;
ALTER TABLE dq_runs DROP COLUMN IF EXISTS observation_id;
