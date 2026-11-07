-- 000028_party_contact_points.down.sql
-- Contact points are governed master data with a verification record. Refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM party_contact_points) THEN
        RAISE EXCEPTION '000028 down refused: party contact points exist';
    END IF;
END
$$;

DROP TABLE IF EXISTS party_contact_points;
DROP FUNCTION IF EXISTS party_contact_point_guard();
