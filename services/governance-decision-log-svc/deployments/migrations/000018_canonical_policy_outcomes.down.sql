-- Migration: 000018_canonical_policy_outcomes.down.sql
DELETE FROM decision_outcome_categories
WHERE outcome IN ('SIGNATURE_REQUIRED', 'NOT_CONFIGURED', 'NOT_IMPLEMENTED', 'ERROR');
