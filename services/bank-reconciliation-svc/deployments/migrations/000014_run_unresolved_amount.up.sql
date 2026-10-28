-- BNK-05: pin the unresolved-amount tolerance on the run, alongside the
-- count and percentage tolerances.
--
-- reconciliation_policies has carried max_unresolved_amount since 000008, but
-- reconciliation_runs had nowhere to pin it, so certification could never
-- apply it. A count/per-cent tolerance alone lets one enormous exception
-- through. The run pins the value at bind time, exactly as it already pins
-- max_unmatched_count, max_unmatched_pct and the policy version.

ALTER TABLE reconciliation_runs
    ADD COLUMN max_unresolved_amount NUMERIC(18,4) NOT NULL DEFAULT 0,
    ADD COLUMN policy_currency VARCHAR(16) NOT NULL DEFAULT '';

-- Backfill every already-bound run from the exact policy version it pinned,
-- so applying this migration does not silently loosen or tighten an
-- in-flight run.
UPDATE reconciliation_runs r
SET max_unresolved_amount = p.max_unresolved_amount,
    policy_currency = p.currency
FROM reconciliation_policies p
WHERE r.policy_id = p.policy_id;
