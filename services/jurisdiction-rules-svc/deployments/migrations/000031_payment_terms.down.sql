-- 000031 down. Payment term versions are the evidence of how a due date was worked
-- out; dropping them loses that, so it is refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM payment_terms) THEN
        RAISE EXCEPTION '000031 down refused: payment terms exist and would be lost';
    END IF;
END
$$;

DROP TABLE IF EXISTS payment_term_versions;
DROP TABLE IF EXISTS payment_terms;
DROP FUNCTION IF EXISTS payment_term_version_guard();
DROP FUNCTION IF EXISTS payment_term_guard();
