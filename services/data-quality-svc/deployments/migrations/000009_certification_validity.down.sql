-- Refused while any certification carries a validity window: dropping the columns would make it reliable forever.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM dq_certifications WHERE valid_until IS NOT NULL) THEN
        RAISE EXCEPTION '000009 down refused: certifications carry a validity window';
    END IF;
END
$$;
ALTER TABLE dq_certifications DROP CONSTRAINT IF EXISTS dq_certifications_validity_positive;
ALTER TABLE dq_certifications DROP CONSTRAINT IF EXISTS dq_certifications_validity_together;
ALTER TABLE dq_certifications DROP COLUMN IF EXISTS valid_until, DROP COLUMN IF EXISTS validity_window_seconds;
