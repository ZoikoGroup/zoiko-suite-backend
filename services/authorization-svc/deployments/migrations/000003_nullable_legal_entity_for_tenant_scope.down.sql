ALTER TABLE delegated_authorities
    ALTER COLUMN legal_entity_id SET NOT NULL;

ALTER TABLE principal_role_assignments
    ALTER COLUMN legal_entity_id SET NOT NULL;
