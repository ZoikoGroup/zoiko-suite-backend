-- Migration 000018 down. A merge is a decision about who a person is; dropping it loses the alias, so it is
-- refused while any merge exists.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM principal_merges) THEN
        RAISE EXCEPTION '000018 down refused: identity merges exist and are evidence';
    END IF;
END
$$;

DROP TABLE IF EXISTS principal_merges;
DROP FUNCTION IF EXISTS principal_merges_guard();
