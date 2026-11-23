-- 000024_dimensions.down.sql
-- Dimension types and values are the identity postings and reports reference. Refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM dimension_types) THEN
        RAISE EXCEPTION '000024 down refused: dimension types exist and are referenced master data';
    END IF;
END
$$;

DROP TABLE IF EXISTS dimension_source_mappings;
DROP TABLE IF EXISTS dimension_value_transitions;
DROP TABLE IF EXISTS dimension_value_versions;
DROP TABLE IF EXISTS dimension_values;
DROP TABLE IF EXISTS dimension_types;
DROP FUNCTION IF EXISTS dimension_transition_guard();
DROP FUNCTION IF EXISTS dimension_type_guard();
DROP FUNCTION IF EXISTS dimension_value_guard();
DROP FUNCTION IF EXISTS dimension_mapping_guard();
DROP FUNCTION IF EXISTS dimension_version_guard();
DROP FUNCTION IF EXISTS dimension_value_entity_guard();
