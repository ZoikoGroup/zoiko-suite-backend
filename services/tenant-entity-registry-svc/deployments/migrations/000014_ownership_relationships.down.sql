-- 000014 down. Drops the ORG-04 relationship table and the key it needed on legal_entities.
DROP TABLE IF EXISTS ownership_relationships;
ALTER TABLE legal_entities DROP CONSTRAINT IF EXISTS le_tenant_entity_unique;
