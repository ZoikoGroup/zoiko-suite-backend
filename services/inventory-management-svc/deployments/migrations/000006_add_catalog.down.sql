DROP TABLE IF EXISTS inventory_catalog_mappings;
DROP TABLE IF EXISTS inventory_catalog_variants;
DROP TABLE IF EXISTS inventory_catalog_offering_versions;
DROP TABLE IF EXISTS inventory_catalog_offerings;
DROP FUNCTION IF EXISTS reject_catalog_mapping_mutation();
DROP FUNCTION IF EXISTS reject_catalog_variant_mutation();
DROP FUNCTION IF EXISTS reject_catalog_version_content_mutation();
