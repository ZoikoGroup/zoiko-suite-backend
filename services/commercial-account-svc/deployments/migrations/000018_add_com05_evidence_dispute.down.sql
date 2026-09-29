-- 000018_add_com05_evidence_dispute.down.sql

DROP TRIGGER IF EXISTS trg_dispute_cases_lifecycle ON dispute_cases;
DROP FUNCTION IF EXISTS enforce_dispute_case_lifecycle();
DROP TABLE IF EXISTS dispute_cases;

DROP TRIGGER IF EXISTS trg_commercial_evidence_packages_immutable ON commercial_evidence_packages;
DROP TABLE IF EXISTS commercial_evidence_packages;
