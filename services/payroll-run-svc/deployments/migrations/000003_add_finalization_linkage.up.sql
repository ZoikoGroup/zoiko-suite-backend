-- payroll_runs could be finalized (status -> COMPLETED) with no recorded
-- link to the governance decision that authorized it, and no integrity hash
-- over the finalized totals — so a regulator/auditor could not verify a
-- payroll run's output was reproducible from stored inputs, only trust the
-- final numbers. Both nullable: governance_decision_id is populated only
-- when the caller supplies one (no fabricated value when none exists yet),
-- and snapshot_hash is computed by this service itself at the moment of
-- finalization, over the run's own already-locked totals — not invented,
-- not caller-supplied.
ALTER TABLE payroll_runs
    ADD COLUMN governance_decision_id TEXT,
    ADD COLUMN snapshot_hash          TEXT;
