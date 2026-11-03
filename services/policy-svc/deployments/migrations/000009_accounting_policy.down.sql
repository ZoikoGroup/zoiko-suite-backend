-- 000009 down. Accounting policies are the evidence of which treatment applied
-- to which book and period; dropping them loses that, so it is refused while
-- any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM accounting_policy_applicability) THEN
        RAISE EXCEPTION '000009 down refused: accounting policies exist and would be lost';
    END IF;
END
$$;

DROP TABLE IF EXISTS accounting_policy_rule_mappings;
DROP TABLE IF EXISTS accounting_policy_version_details;
DROP TABLE IF EXISTS accounting_policy_applicability;
