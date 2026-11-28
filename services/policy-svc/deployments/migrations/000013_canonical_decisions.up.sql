-- Migration: 000013_canonical_decisions.up.sql
--
-- ZS-GOV-INT-002 (Document 02) canonical decisions, stored as evidence beside
-- the evaluations they extend:
--
--   * A decision no rule produced (NOT_CONFIGURED: no matrix, overlapping
--     matrices; NOT_IMPLEMENTED) is evidence too (Doc 01 §1.2: every decision
--     carries its reason and an immutable evidence reference), so its policy
--     provenance may be absent. The CHECK keeps that to non-permissive
--     results only: a PERMIT or a *_REQUIRED decision always names the version
--     that produced it.
--   * contract_version, decision_outcome, reason_codes and valid_until are the
--     envelope's queryable fields (metrics: NOT_CONFIGURED by tenant/entity).
--   * decision_envelope is the signed envelope exactly as answered, so a
--     replay and a verifier get the same bytes the caller got. It is TEXT, not
--     JSONB: JSONB re-orders keys and whitespace, and a replay must be
--     byte-identical (Doc 02 §9.1), not merely equivalent.
--
-- Rows stay append-only (trg_policy_evaluations_append_only).

ALTER TABLE policy_evaluations
    ALTER COLUMN policy_id DROP NOT NULL,
    ALTER COLUMN policy_version_id DROP NOT NULL,
    ALTER COLUMN policy_code DROP NOT NULL,
    ALTER COLUMN artifact_digest DROP NOT NULL,
    ADD COLUMN contract_version  VARCHAR(16),
    ADD COLUMN decision_outcome  VARCHAR(32),
    ADD COLUMN reason_codes      TEXT[]       NOT NULL DEFAULT '{}',
    ADD COLUMN valid_until       TIMESTAMPTZ,
    ADD COLUMN decision_envelope TEXT;

ALTER TABLE policy_evaluations
    ADD CONSTRAINT chk_policy_evaluations_provenance
        CHECK (policy_version_id IS NOT NULL
               OR result IN ('NOT_CONFIGURED', 'NOT_IMPLEMENTED', 'ERROR')),
    ADD CONSTRAINT chk_policy_evaluations_decision_outcome
        CHECK (decision_outcome IS NULL OR decision_outcome IN (
            'PERMIT', 'APPROVAL_REQUIRED', 'SIGNATURE_REQUIRED', 'REVIEW_REQUIRED',
            'DENY', 'NOT_CONFIGURED', 'NOT_IMPLEMENTED', 'ERROR')),
    ADD CONSTRAINT chk_policy_evaluations_envelope_signed
        CHECK (decision_envelope IS NULL OR decision_envelope::jsonb ? 'integrity');

CREATE INDEX idx_policy_evaluations_outcome
    ON policy_evaluations (tenant_id, policy_type, decision_outcome, evaluated_at DESC);
