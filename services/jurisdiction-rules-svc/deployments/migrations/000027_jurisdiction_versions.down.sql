-- 000027 down. The version history is evidence of what the hierarchy looked like
-- on a past date; dropping it loses that, so it is refused once any jurisdiction
-- has more than one version or has been retired with its evidence.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM jurisdiction_versions WHERE version_no > 1)
       OR EXISTS (SELECT 1 FROM jurisdictions
                   WHERE retirement_reason IS NOT NULL OR retirement_evidence_ref IS NOT NULL
                      OR superseded_by_jurisdiction_id IS NOT NULL) THEN
        RAISE EXCEPTION '000027 down refused: jurisdiction history or retirement evidence exists and would be lost';
    END IF;
END
$$;

DROP TRIGGER IF EXISTS trg_jurisdiction_record_version ON jurisdictions;
DROP FUNCTION IF EXISTS jurisdiction_record_version();
DROP TABLE IF EXISTS jurisdiction_versions;
DROP FUNCTION IF EXISTS jurisdiction_versions_guard();

ALTER TABLE jurisdictions DROP CONSTRAINT IF EXISTS ck_jurisdiction_successor;
ALTER TABLE jurisdictions
    DROP COLUMN IF EXISTS superseded_by_jurisdiction_id,
    DROP COLUMN IF EXISTS retirement_evidence_ref,
    DROP COLUMN IF EXISTS retirement_reason;
