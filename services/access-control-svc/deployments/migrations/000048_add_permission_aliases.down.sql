-- Migration 000048 down: remove alias_for column from permission_definitions.

ALTER TABLE permission_definitions DROP CONSTRAINT IF EXISTS permission_definitions_alias_naming;
ALTER TABLE permission_definitions DROP CONSTRAINT IF EXISTS permission_definitions_alias_for_fk;
ALTER TABLE permission_definitions DROP COLUMN IF EXISTS alias_for;