-- 000025_units_of_measure.down.sql
-- Units and their conversion versions are what quantity-to-money calculations pin. Refused while any dimension exists.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM quantity_dimensions) THEN
        RAISE EXCEPTION '000025 down refused: quantity dimensions exist and are referenced master data';
    END IF;
END
$$;

DROP TABLE IF EXISTS conversion_versions;
DROP TABLE IF EXISTS units;
DROP TABLE IF EXISTS quantity_dimensions;
DROP FUNCTION IF EXISTS conversion_version_guard();
DROP FUNCTION IF EXISTS unit_guard();
DROP FUNCTION IF EXISTS quantity_dimension_guard();
