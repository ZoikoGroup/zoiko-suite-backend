-- ZS-SVC-Z-001 INV-04 / INV-05: a rule whose evidence is missing is
-- INDETERMINATE, never Pass. dq_results.status previously admitted only
-- 'Pass' and 'Fail', so missing evidence had no representation and a run that
-- reported one rule out of N could be certified.
--
-- Only the CHECK changes. dq_results stays append-only (the immutability
-- trigger from 000001 is untouched) and RLS is unaffected.

ALTER TABLE dq_results DROP CONSTRAINT IF EXISTS dq_results_status_check;
ALTER TABLE dq_results
    ADD CONSTRAINT dq_results_status_check
    CHECK (status IN ('Pass', 'Fail', 'Indeterminate'));

-- An Indeterminate result reports no evidence at all: it cannot carry pass or
-- fail counts, or the "indeterminate" label would hide a real outcome.
-- (Outcomes that evaluated zero records are the same case: 0 and 0.)
ALTER TABLE dq_results
    ADD CONSTRAINT dq_results_indeterminate_has_no_counts
    CHECK (status <> 'Indeterminate' OR (pass_count = 0 AND fail_count = 0));

-- Operators triage Indeterminate results the same way as failures.
CREATE INDEX IF NOT EXISTS idx_dq_results_indeterminate
    ON dq_results(tenant_id, run_id) WHERE status = 'Indeterminate';
