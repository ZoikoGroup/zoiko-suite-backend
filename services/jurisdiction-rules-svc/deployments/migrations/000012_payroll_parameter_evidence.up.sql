-- 000012_payroll_parameter_evidence.up.sql
-- ZS-JUR-001 Wave 6 (payroll): payroll statutory parameter set resolutions
-- share the decision evidence ledger. A set can span several packs, so its
-- evidence is anchored on the digest of the whole set (rule_content_digest);
-- each item in the stored response carries its own pack release and artifact
-- digest, so the exact parameters a payroll run was given can be reproduced.

ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION', 'PARAMETER_SET')),
    ADD CONSTRAINT ck_decision_params_have_digest CHECK (outcome <> 'PARAMETERS_RESOLVED' OR rule_content_digest IS NOT NULL);
