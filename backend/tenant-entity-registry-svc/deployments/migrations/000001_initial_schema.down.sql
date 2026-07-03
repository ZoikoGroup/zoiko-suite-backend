ALTER TABLE legal_entities DROP CONSTRAINT IF EXISTS fk_legal_entities_tax_bundle;

DROP TABLE IF EXISTS tax_identity_bundles;
DROP TABLE IF EXISTS entity_jurisdiction_assignments;
DROP TABLE IF EXISTS entity_hierarchies;
DROP TABLE IF EXISTS legal_entities;
DROP TABLE IF EXISTS data_residency_policies;
DROP TABLE IF EXISTS tenants;
DROP TABLE IF EXISTS residency_regions;
