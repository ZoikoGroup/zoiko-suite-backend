-- Migration: 000018_canonical_policy_outcomes.up.sql
--
-- ZS-GOV-INT-002 (Document 02) §2.2 outcome vocabulary. policy-svc records
-- signatory-matrix decisions under their canonical outcome, so the ledger
-- needs a status category for the four spellings 000016 did not list. Every
-- non-permissive outcome stays out of APPROVED: a hold is ESCALATED, a
-- configuration gap, a missing capability or a runtime error is DENIED
-- (blocked), never "other" and never approved (Doc 01 §8.1).
INSERT INTO decision_outcome_categories (outcome, category) VALUES
    ('SIGNATURE_REQUIRED', 'ESCALATED'),
    ('NOT_CONFIGURED', 'DENIED'),
    ('NOT_IMPLEMENTED', 'DENIED'),
    ('ERROR', 'DENIED')
ON CONFLICT (outcome) DO NOTHING;
