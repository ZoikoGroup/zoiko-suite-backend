-- Migration: 000032_account_source_policy_ref.up.sql
--
-- ACC-01 SPEC_DEVIATIONS A8/B18: "source policy" is named in the spec's
-- evidence/lineage line ("Exact account version, approver, reason,
-- effective dates and source policy retained with every material change")
-- but was never captured anywhere, not even as an optional field. The spec
-- gives it no shape (no type, no reference target), so this is a free-text
-- reference, the same posture evidence_refs already takes — not a foreign
-- key into a policy table this service has no model for.
--
-- Nullable and additive: no existing account_versions/account_change_approvals
-- row is affected, and no existing caller is required to supply it.
ALTER TABLE account_versions
    ADD COLUMN source_policy_ref TEXT;

ALTER TABLE account_change_approvals
    ADD COLUMN source_policy_ref TEXT;
