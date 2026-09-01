DROP TRIGGER IF EXISTS trg_reject_recovery_commitments_mutation ON recovery_commitments;
DROP TRIGGER IF EXISTS trg_reject_recovery_applications_mutation ON recovery_applications;
DROP FUNCTION IF EXISTS reject_recovery_evidence_mutation();
DROP TRIGGER IF EXISTS trg_reject_recovery_case_mutation ON supplier_recovery_cases;
DROP FUNCTION IF EXISTS reject_recovery_case_mutation();
