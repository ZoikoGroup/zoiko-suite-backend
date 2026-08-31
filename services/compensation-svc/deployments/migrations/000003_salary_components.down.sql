-- Reverses 000003_salary_components.

DROP INDEX IF EXISTS idx_structure_components_structure;
DROP INDEX IF EXISTS idx_salary_components_tenant_entity;
DROP INDEX IF EXISTS idx_salary_components_entity_code;

DROP POLICY IF EXISTS tenant_isolation_structure_components ON structure_components;
DROP POLICY IF EXISTS tenant_isolation_salary_components ON salary_components;

-- structure_components references salary_components, so it goes first.
DROP TABLE IF EXISTS structure_components;
DROP TABLE IF EXISTS salary_components;
