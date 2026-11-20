-- 000005_add_capability_claim_approval_workflow.up.sql
--
-- Gap 1 fix (2026-11-19 audit): capability_claims' SoD check was two
-- free-text columns (wording_owner_principal_id, approved_by_principal_id)
-- that only had to differ textually from each other — neither was ever
-- checked against the authenticated caller or a real principal record, so
-- a single caller could satisfy it with two made-up strings.
--
-- Replaces that with the same proposer/decider split
-- ai-governance-svc's automation_actions/policy_change_approvals use:
-- wording_owner_principal_id is now always the creator (enforced in the
-- handler, not the schema — Postgres has no way to know who the
-- "authenticated caller" was), and approval is a separate later decision by
-- a different principal, recorded here.

ALTER TABLE capability_claims
    ALTER COLUMN approved_by_principal_id DROP NOT NULL;

ALTER TABLE capability_claims
    ADD COLUMN approval_status VARCHAR(16) NOT NULL DEFAULT 'PENDING',
    ADD COLUMN approved_at TIMESTAMP WITH TIME ZONE;

-- Existing rows (pre-Gap-1) already carried an approved_by_principal_id
-- supplied at creation time under the old, unverified contract. Treat them
-- as already decided/APPROVED rather than retroactively inventing a
-- decision event none of them actually had the real workflow for — this is
-- a schema migration, not a claim to have re-verified historical approvals.
-- This MUST run before the consistency CHECK below is added, or adding that
-- constraint would fail validation against these very rows.
UPDATE capability_claims
SET approval_status = 'APPROVED', approved_at = created_at
WHERE approved_by_principal_id IS NOT NULL;

ALTER TABLE capability_claims
    ADD CONSTRAINT capability_claims_approval_status_valid
        CHECK (approval_status IN ('PENDING', 'APPROVED', 'REJECTED'));

-- A decided claim must carry who decided it and when; a pending one must
-- carry neither yet. Keeps the three approval columns from drifting out of
-- sync with each other at the database level, not just in application code.
ALTER TABLE capability_claims
    ADD CONSTRAINT capability_claims_approval_consistency
        CHECK (
            (approval_status = 'PENDING' AND approved_by_principal_id IS NULL AND approved_at IS NULL)
            OR (approval_status IN ('APPROVED', 'REJECTED') AND approved_by_principal_id IS NOT NULL AND approved_at IS NOT NULL)
        );
