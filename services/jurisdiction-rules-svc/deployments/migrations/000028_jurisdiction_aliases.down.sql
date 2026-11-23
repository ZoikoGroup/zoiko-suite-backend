-- 000028 down. The alias mappings are evidence of what a code meant on a past
-- date; dropping them loses that, so it is refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM jurisdiction_aliases) THEN
        RAISE EXCEPTION '000028 down refused: jurisdiction alias mappings exist and would be lost';
    END IF;
END
$$;

DROP TABLE IF EXISTS jurisdiction_aliases;
DROP FUNCTION IF EXISTS jurisdiction_aliases_guard();
