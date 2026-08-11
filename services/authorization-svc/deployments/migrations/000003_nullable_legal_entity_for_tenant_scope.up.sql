-- principal_role_assignments.legal_entity_id was NOT NULL, which made it
-- impossible to ever grant a TENANT-scoped role (roles.role_scope_type
-- already supports 'TENANT' as a value) tenant-wide — every assignment was
-- forced to name one specific entity regardless of what the role itself
-- claimed to be scoped to. NULL now means "applies across every legal
-- entity in the tenant", evaluated as such in FindGrantedActions.
--
-- delegated_authorities.legal_entity_id had the identical problem: no way
-- to delegate authority across a whole tenant rather than one entity.
-- Same NULL convention, evaluated in FindDelegatedActions.
ALTER TABLE principal_role_assignments
    ALTER COLUMN legal_entity_id DROP NOT NULL;

ALTER TABLE delegated_authorities
    ALTER COLUMN legal_entity_id DROP NOT NULL;
