-- Migration 000016 down. Dropping the columns un-suspends every suspended
-- assignment, so it is refused while any is suspended.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM principal_role_assignments
                WHERE suspended_at IS NOT NULL AND (resumed_at IS NULL OR suspended_at > resumed_at)) THEN
        RAISE EXCEPTION '000016 down refused: suspended role assignments exist and would start being framed again';
    END IF;
END
$$;

ALTER TABLE principal_role_assignments
    DROP COLUMN IF EXISTS resumed_at,
    DROP COLUMN IF EXISTS suspended_at;
